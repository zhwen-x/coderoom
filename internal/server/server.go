// Package server is the teacher-side service: an HTTP API for student clients,
// an HTTP API plus embedded web UI for the teacher, a judge worker pool, and a
// UDP responder so clients can find the server without being told its IP.
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"coderoom/internal/judge"
	"coderoom/internal/protocol"
	"coderoom/internal/store"
)

//go:embed all:web
var webFS embed.FS

const ClientVersion = "0.1.0"

// Config is persisted to <dataDir>/config.json so a restart keeps the same
// admin token (otherwise every restart would invalidate the teacher's open tab).
type Config struct {
	Addr          string          `json:"addr"`
	DiscoveryPort int             `json:"discovery_port"`
	Workers       int             `json:"workers"`
	AdminToken    string          `json:"admin_token"`
	Toolchain     judge.Toolchain `json:"toolchain_override"`
}

func DefaultConfig() Config {
	return Config{
		Addr:          ":8080",
		DiscoveryPort: 8099,
		Workers:       2,
	}
}

type Server struct {
	cfg     Config
	cfgPath string
	st      *store.Store
	judger  *judge.Judger
	baseDir string // directory of the executable: where a bundled toolchain lives
	mux     *http.ServeMux
	logger  *log.Logger
}

// New wires the server. baseDir is where a bundled toolchain is looked for.
func New(st *store.Store, cfg Config, baseDir string, logger *log.Logger) (*Server, error) {
	if logger == nil {
		logger = log.Default()
	}
	cfgPath := filepath.Join(st.DataDir(), "config.json")

	if cfg.AdminToken == "" {
		tok, err := randomToken()
		if err != nil {
			return nil, err
		}
		cfg.AdminToken = tok
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}

	tc := judge.DetectToolchain(baseDir, cfg.Toolchain)
	s := &Server{
		cfg:     cfg,
		cfgPath: cfgPath,
		st:      st,
		judger:  judge.New(tc, judge.DefaultRunner{}, filepath.Join(st.DataDir(), "work")),
		baseDir: baseDir,
		logger:  logger,
	}
	if err := s.saveConfig(); err != nil {
		return nil, err
	}
	s.routes()
	return s, nil
}

func randomToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// LoadConfig reads the persisted config, falling back to defaults.
func LoadConfig(dataDir string) Config {
	cfg := DefaultConfig()
	b, err := os.ReadFile(filepath.Join(dataDir, "config.json"))
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(b, &cfg)
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	if cfg.DiscoveryPort == 0 {
		cfg.DiscoveryPort = 8099
	}
	return cfg
}

func (s *Server) saveConfig() error {
	b, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.cfgPath, b, 0o600)
}

func (s *Server) Config() Config { return s.cfg }

// AdminURL is what the teacher opens; the token is in the query string so a
// single click from the console is enough to get in.
func (s *Server) AdminURL() string {
	port := s.cfg.Addr
	if strings.HasPrefix(port, ":") {
		port = "localhost" + port
	}
	return fmt.Sprintf("http://%s/?t=%s", port, s.cfg.AdminToken)
}

func (s *Server) routes() {
	mux := http.NewServeMux()

	// ---- student client API ------------------------------------------------
	mux.HandleFunc("GET /api/client/exams", s.handleClientExams)
	mux.HandleFunc("POST /api/client/login", s.handleClientLogin)
	mux.HandleFunc("GET /api/client/paper", s.auth(s.handleClientPaper))
	mux.HandleFunc("POST /api/client/snapshot", s.auth(s.handleClientSnapshot))
	mux.HandleFunc("POST /api/client/submit", s.auth(s.handleClientSubmit))
	mux.HandleFunc("POST /api/client/heartbeat", s.auth(s.handleClientHeartbeat))
	mux.HandleFunc("POST /api/client/events", s.auth(s.handleClientEvents))
	mux.HandleFunc("POST /api/client/handin", s.auth(s.handleClientHandin))
	mux.HandleFunc("GET /api/client/results", s.auth(s.handleClientResults))

	// ---- teacher API -------------------------------------------------------
	mux.HandleFunc("GET /api/teacher/system", s.admin(s.handleSystem))
	mux.HandleFunc("PUT /api/teacher/system/toolchain", s.admin(s.handleSetToolchain))

	mux.HandleFunc("GET /api/teacher/problems", s.admin(s.handleListProblems))
	mux.HandleFunc("POST /api/teacher/problems", s.admin(s.handleCreateProblem))
	mux.HandleFunc("GET /api/teacher/problems/{id}", s.admin(s.handleGetProblem))
	mux.HandleFunc("PUT /api/teacher/problems/{id}", s.admin(s.handleUpdateProblem))
	mux.HandleFunc("DELETE /api/teacher/problems/{id}", s.admin(s.handleDeleteProblem))
	mux.HandleFunc("GET /api/teacher/problems/{id}/testcases", s.admin(s.handleGetTestcases))
	mux.HandleFunc("PUT /api/teacher/problems/{id}/testcases", s.admin(s.handlePutTestcases))

	mux.HandleFunc("GET /api/teacher/exams", s.admin(s.handleListExams))
	mux.HandleFunc("POST /api/teacher/exams", s.admin(s.handleCreateExam))
	mux.HandleFunc("GET /api/teacher/exams/{id}", s.admin(s.handleGetExam))
	mux.HandleFunc("DELETE /api/teacher/exams/{id}", s.admin(s.handleDeleteExam))
	mux.HandleFunc("POST /api/teacher/exams/{id}/state", s.admin(s.handleSetExamState))
	mux.HandleFunc("PUT /api/teacher/exams/{id}/paper", s.admin(s.handleSetExamPaper))
	mux.HandleFunc("POST /api/teacher/exams/{id}/judge", s.admin(s.handleJudgeExam))
	mux.HandleFunc("POST /api/teacher/exams/{id}/release", s.admin(s.handleReleaseResults))
	mux.HandleFunc("GET /api/teacher/exams/{id}/sessions", s.admin(s.handleExamSessions))
	mux.HandleFunc("GET /api/teacher/exams/{id}/submissions", s.admin(s.handleExamSubmissions))
	mux.HandleFunc("GET /api/teacher/exams/{id}/scores", s.admin(s.handleExamScores))
	mux.HandleFunc("GET /api/teacher/exams/{id}/scores.csv", s.admin(s.handleExamScoresCSV))
	mux.HandleFunc("GET /api/teacher/exams/{id}/events", s.admin(s.handleExamEvents))

	mux.HandleFunc("GET /api/teacher/submissions/{id}", s.admin(s.handleGetSubmission))
	mux.HandleFunc("POST /api/teacher/submissions/{id}/judge", s.admin(s.handleRejudge))

	mux.HandleFunc("POST /api/teacher/students/import", s.admin(s.handleImportStudents))
	mux.HandleFunc("GET /api/teacher/students", s.admin(s.handleListStudents))

	// ---- teacher web UI ----------------------------------------------------
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err) // embedded FS layout is a build-time fact
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	s.mux = mux
}

func (s *Server) Handler() http.Handler { return s.logRequests(s.mux) }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health-style chatter (heartbeats, snapshots) would drown the log, so
		// only non-routine paths are recorded.
		noisy := strings.HasPrefix(r.URL.Path, "/api/client/heartbeat") ||
			strings.HasPrefix(r.URL.Path, "/api/client/snapshot") ||
			strings.HasPrefix(r.URL.Path, "/api/client/results")
		if !noisy && strings.HasPrefix(r.URL.Path, "/api/") {
			s.logger.Printf("%s %s from %s", r.Method, r.URL.Path, clientIP(r))
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- middleware

// admin guards every teacher endpoint. Without this the teacher API would be
// reachable from any student machine on the LAN, which would hand out the test
// data and everyone else's code.
func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("X-Admin-Token")
		if tok == "" {
			tok = r.URL.Query().Get("t")
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.AdminToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "管理口令无效")
			return
		}
		next(w, r)
	}
}

func (s *Server) auth(next func(http.ResponseWriter, *http.Request, *store.Session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		sess, err := s.st.SessionByToken(r.Context(), token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "会话失效，请重新登录")
			return
		}
		next(w, r, sess)
	}
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	return ""
}

// ------------------------------------------------------------------- helpers

func (s *Server) envelope(policyVersion int) protocol.Envelope {
	return protocol.Envelope{ServerTime: store.NowMS(), PolicyVersion: policyVersion}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, protocol.ErrorResponse{Error: code, Detail: detail})
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<20))
	return dec.Decode(v)
}

func pathID(r *http.Request, name string) (int64, error) {
	return strconv.ParseInt(r.PathValue(name), 10, 64)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// LocalIPs lists the LAN addresses a student client can reach, for the
// startup banner and the teacher's "tell students this address" panel.
func LocalIPs() []string {
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if ip4 := ipn.IP.To4(); ip4 != nil {
					out = append(out, ip4.String())
				}
			}
		}
	}
	return out
}

// Port extracts the numeric port the server listens on.
func (s *Server) Port() int {
	_, p, err := net.SplitHostPort(s.cfg.Addr)
	if err != nil {
		return 8080
	}
	n, _ := strconv.Atoi(p)
	if n == 0 {
		return 8080
	}
	return n
}

// Run starts the HTTP listener, the judge workers and the discovery responder,
// and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	if n, err := s.st.RequeueStaleTasks(ctx); err != nil {
		s.logger.Printf("warning: could not requeue stale judge tasks: %v", err)
	} else if n > 0 {
		s.logger.Printf("requeued %d judge task(s) interrupted by a previous shutdown", n)
	}

	for i := 0; i < s.cfg.Workers; i++ {
		go s.judgeWorker(ctx, fmt.Sprintf("worker-%d", i+1))
	}
	go s.serveDiscovery(ctx)

	srv := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

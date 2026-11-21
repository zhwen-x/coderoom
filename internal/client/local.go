package client

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"coderoom/internal/protocol"
)

// Draft is one problem's answer as it stands on this machine.
type Draft struct {
	ProblemID    int64  `json:"problem_id"`
	Language     string `json:"language"`
	Code         string `json:"code"`
	UpdatedAt    int64  `json:"updated_at"`
	UploadedHash string `json:"uploaded_hash"` // last hash the server acknowledged
}

// Dirty reports whether this draft still differs from what the server has.
func (d *Draft) Dirty() bool { return d.UploadedHash != HashCode(d.Code) }

// LocalState is everything worth surviving a client restart.
//
// Problem statements are deliberately NOT persisted: keeping them in memory
// only means a network outage mid-exam does not lose the paper, while nothing
// readable is left on disk for a student to dig out before the exam starts.
type LocalState struct {
	ServerAddr    string           `json:"server_addr"`
	ExamID        int64            `json:"exam_id"`
	StudentNo     string           `json:"student_no"`
	Name          string           `json:"name"`
	Drafts        map[int64]*Draft `json:"drafts"`
	PendingEvents []protocol.Event `json:"pending_events"`
	UpdatedAt     int64            `json:"updated_at"`
}

// LocalStore persists LocalState to a single JSON file.
//
// Every save is a write-to-temp-then-rename, so a crash or a power cut in the
// middle of a save leaves the previous good file rather than a truncated one.
type LocalStore struct {
	mu   sync.Mutex
	path string
	st   LocalState
}

func OpenLocalStore(dataDir string) (*LocalStore, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	ls := &LocalStore{
		path: filepath.Join(dataDir, "session.json"),
		st:   LocalState{Drafts: map[int64]*Draft{}},
	}
	if b, err := os.ReadFile(ls.path); err == nil {
		var loaded LocalState
		if json.Unmarshal(b, &loaded) == nil {
			if loaded.Drafts == nil {
				loaded.Drafts = map[int64]*Draft{}
			}
			ls.st = loaded
		}
	}
	return ls, nil
}

func (ls *LocalStore) State() LocalState {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.st.clone()
}

func (s LocalState) clone() LocalState {
	out := s
	out.Drafts = make(map[int64]*Draft, len(s.Drafts))
	for k, v := range s.Drafts {
		cp := *v
		out.Drafts[k] = &cp
	}
	out.PendingEvents = append([]protocol.Event(nil), s.PendingEvents...)
	return out
}

// Update mutates the state under lock and persists it.
func (ls *LocalStore) Update(fn func(*LocalState)) error {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	fn(&ls.st)
	ls.st.UpdatedAt = time.Now().UnixMilli()
	return ls.saveLocked()
}

func (ls *LocalStore) saveLocked() error {
	b, err := json.MarshalIndent(ls.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := ls.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ls.path)
}

// SaveDraft records the student's current code for one problem.
func (ls *LocalStore) SaveDraft(problemID int64, language, code string) error {
	return ls.Update(func(st *LocalState) {
		d := st.Drafts[problemID]
		if d == nil {
			d = &Draft{ProblemID: problemID}
			st.Drafts[problemID] = d
		}
		d.Language = language
		d.Code = code
		d.UpdatedAt = time.Now().UnixMilli()
	})
}

// MarkUploaded records that the server has accepted this exact content, so the
// next snapshot tick can skip it.
func (ls *LocalStore) MarkUploaded(problemID int64, hash string) {
	_ = ls.Update(func(st *LocalState) {
		if d := st.Drafts[problemID]; d != nil {
			d.UploadedHash = hash
		}
	})
}

// AdoptServerDrafts seeds local drafts from what the server had, for a student
// resuming on a fresh machine. A newer local draft always wins: the student's
// own machine is the more recent source of truth.
func (ls *LocalStore) AdoptServerDrafts(drafts []protocol.Draft) {
	_ = ls.Update(func(st *LocalState) {
		for _, sd := range drafts {
			local := st.Drafts[sd.ProblemID]
			if local != nil && local.UpdatedAt >= sd.UpdatedAt {
				continue
			}
			st.Drafts[sd.ProblemID] = &Draft{
				ProblemID:    sd.ProblemID,
				Language:     sd.Language,
				Code:         sd.Code,
				UpdatedAt:    sd.UpdatedAt,
				UploadedHash: HashCode(sd.Code),
			}
		}
	})
}

// QueueEvent appends a behaviour event to the offline queue. Events are queued
// rather than sent inline so a network blip never drops evidence.
func (ls *LocalStore) QueueEvent(ev protocol.Event) {
	const maxQueued = 2000 // bound the file if the server is down for hours
	_ = ls.Update(func(st *LocalState) {
		st.PendingEvents = append(st.PendingEvents, ev)
		if len(st.PendingEvents) > maxQueued {
			st.PendingEvents = st.PendingEvents[len(st.PendingEvents)-maxQueued:]
		}
	})
}

// TakeEvents removes up to n queued events for sending. On failure the caller
// puts them back with ReturnEvents.
func (ls *LocalStore) TakeEvents(n int) []protocol.Event {
	var taken []protocol.Event
	_ = ls.Update(func(st *LocalState) {
		if len(st.PendingEvents) == 0 {
			return
		}
		if n > len(st.PendingEvents) {
			n = len(st.PendingEvents)
		}
		taken = append(taken, st.PendingEvents[:n]...)
		st.PendingEvents = st.PendingEvents[n:]
	})
	return taken
}

func (ls *LocalStore) ReturnEvents(evs []protocol.Event) {
	_ = ls.Update(func(st *LocalState) {
		st.PendingEvents = append(evs, st.PendingEvents...)
	})
}

// ExportAnswers writes every draft out as plain source files. This is the
// fallback when the network fails at the end of an exam: the teacher can
// collect this folder by hand rather than losing the student's work.
func (ls *LocalStore) ExportAnswers(dir string, titles map[int64]string) (string, error) {
	st := ls.State()
	out := filepath.Join(dir, fmt.Sprintf("answers-%s-%s",
		sanitizeFileName(st.StudentNo), time.Now().Format("20060102-150405")))
	if err := os.MkdirAll(out, 0o755); err != nil {
		return "", err
	}
	for id, d := range st.Drafts {
		name := fmt.Sprintf("p%d", id)
		if t := titles[id]; t != "" {
			name += "-" + sanitizeFileName(t)
		}
		name += "." + SourceExt(d.Language)
		if err := os.WriteFile(filepath.Join(out, name), []byte(d.Code), 0o644); err != nil {
			return "", err
		}
	}
	meta, _ := json.MarshalIndent(map[string]any{
		"student_no":  st.StudentNo,
		"name":        st.Name,
		"exam_id":     st.ExamID,
		"exported_at": time.Now().Format(time.RFC3339),
	}, "", "  ")
	_ = os.WriteFile(filepath.Join(out, "answers.json"), meta, 0o644)
	return out, nil
}

func sanitizeFileName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', '\n', '\r', '\t':
			out = append(out, '_')
		default:
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return "unknown"
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return string(out)
}

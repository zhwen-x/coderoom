-- CodeRoom schema. All timestamps are unix milliseconds.
--
-- The full table set is created up front even though M0 only exercises part of
-- it: creating an empty table is free, and it keeps later milestones from
-- needing schema migrations mid-project.

CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS students (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  student_no TEXT NOT NULL UNIQUE,
  name       TEXT NOT NULL,
  class      TEXT NOT NULL DEFAULT '',
  batch      TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS problems (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  title           TEXT NOT NULL,
  description_md  TEXT NOT NULL DEFAULT '',
  input_desc      TEXT NOT NULL DEFAULT '',
  output_desc     TEXT NOT NULL DEFAULT '',
  samples_json    TEXT NOT NULL DEFAULT '[]',
  time_limit_ms   INTEGER NOT NULL DEFAULT 1000,
  memory_limit_mb INTEGER NOT NULL DEFAULT 256,
  difficulty      INTEGER NOT NULL DEFAULT 2,
  tags            TEXT NOT NULL DEFAULT '',
  languages       TEXT NOT NULL DEFAULT 'cpp,c,python',
  score_mode      TEXT NOT NULL DEFAULT 'sum',  -- sum | allornothing
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS testcases (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  problem_id  INTEGER NOT NULL REFERENCES problems(id) ON DELETE CASCADE,
  idx         INTEGER NOT NULL,
  input_path  TEXT NOT NULL,
  output_path TEXT NOT NULL,
  score       INTEGER NOT NULL DEFAULT 10,
  is_sample   INTEGER NOT NULL DEFAULT 0,
  UNIQUE(problem_id, idx)
);

-- Paper templates hold the *rules*; papers hold a concrete generated list.
-- Keeping them apart is what lets one template produce either a single
-- class-wide paper or one distinct paper per student.
CREATE TABLE IF NOT EXISTS paper_templates (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  name        TEXT NOT NULL,
  mode        TEXT NOT NULL DEFAULT 'fixed',
  total_score INTEGER NOT NULL DEFAULT 100,
  rules_json  TEXT NOT NULL DEFAULT '{}',
  created_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS template_items (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  template_id      INTEGER NOT NULL REFERENCES paper_templates(id) ON DELETE CASCADE,
  idx              INTEGER NOT NULL,
  pick_type        TEXT NOT NULL DEFAULT 'fixed',  -- fixed | pool | filter
  fixed_problem_id INTEGER,
  pool_json        TEXT NOT NULL DEFAULT '[]',
  filter_json      TEXT NOT NULL DEFAULT '{}',
  score            INTEGER NOT NULL DEFAULT 10
);

CREATE TABLE IF NOT EXISTS exams (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  name              TEXT NOT NULL,
  type              TEXT NOT NULL DEFAULT 'practice',  -- practice | exam
  paper_template_id INTEGER,
  per_student_paper INTEGER NOT NULL DEFAULT 0,
  start_at          INTEGER,
  end_at            INTEGER,
  duration_min      INTEGER NOT NULL DEFAULT 0,
  roster_required   INTEGER NOT NULL DEFAULT 0,
  passcode          TEXT NOT NULL DEFAULT '',
  show_results      INTEGER NOT NULL DEFAULT 1,
  allow_rejudge     INTEGER NOT NULL DEFAULT 1,
  policy_json       TEXT NOT NULL DEFAULT '{}',
  policy_version    INTEGER NOT NULL DEFAULT 1,
  state             TEXT NOT NULL DEFAULT 'draft',
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS papers (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  exam_id      INTEGER NOT NULL REFERENCES exams(id) ON DELETE CASCADE,
  template_id  INTEGER,
  scope        TEXT NOT NULL DEFAULT 'class',  -- class | student
  student_no   TEXT NOT NULL DEFAULT '',
  generated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_papers_exam ON papers(exam_id, scope, student_no);

CREATE TABLE IF NOT EXISTS paper_items (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  paper_id   INTEGER NOT NULL REFERENCES papers(id) ON DELETE CASCADE,
  idx        INTEGER NOT NULL,
  problem_id INTEGER NOT NULL REFERENCES problems(id),
  score      INTEGER NOT NULL DEFAULT 10,
  UNIQUE(paper_id, idx)
);

CREATE TABLE IF NOT EXISTS sessions (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  exam_id           INTEGER NOT NULL REFERENCES exams(id) ON DELETE CASCADE,
  student_no        TEXT NOT NULL,
  name              TEXT NOT NULL,
  class             TEXT NOT NULL DEFAULT '',
  machine_name      TEXT NOT NULL DEFAULT '',
  ip                TEXT NOT NULL DEFAULT '',
  mac               TEXT NOT NULL DEFAULT '',
  os                TEXT NOT NULL DEFAULT '',
  client_version    TEXT NOT NULL DEFAULT '',
  token             TEXT NOT NULL UNIQUE,
  paper_id          INTEGER,
  login_at          INTEGER NOT NULL,
  last_heartbeat_at INTEGER NOT NULL,
  submitted_at      INTEGER,
  UNIQUE(exam_id, student_no)
);

CREATE TABLE IF NOT EXISTS submissions (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id    INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  exam_id       INTEGER NOT NULL,
  student_no    TEXT NOT NULL,
  problem_id    INTEGER NOT NULL,
  paper_item_id INTEGER,
  language      TEXT NOT NULL,
  kind          TEXT NOT NULL,              -- snapshot | final
  seq           INTEGER NOT NULL DEFAULT 1,
  code_path     TEXT NOT NULL,
  code_hash     TEXT NOT NULL,
  code_bytes    INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sub_session ON submissions(session_id, problem_id, kind, id DESC);
CREATE INDEX IF NOT EXISTS idx_sub_exam    ON submissions(exam_id, kind, id DESC);

CREATE TABLE IF NOT EXISTS judge_tasks (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  submission_id INTEGER NOT NULL UNIQUE REFERENCES submissions(id) ON DELETE CASCADE,
  priority      INTEGER NOT NULL DEFAULT 0,
  state         TEXT NOT NULL DEFAULT 'pending',  -- pending | running | done | error
  worker        TEXT NOT NULL DEFAULT '',
  attempts      INTEGER NOT NULL DEFAULT 0,
  error         TEXT NOT NULL DEFAULT '',
  enqueued_at   INTEGER NOT NULL,
  started_at    INTEGER,
  finished_at   INTEGER
);
CREATE INDEX IF NOT EXISTS idx_task_state ON judge_tasks(state, priority DESC, id);

CREATE TABLE IF NOT EXISTS judge_results (
  submission_id INTEGER PRIMARY KEY REFERENCES submissions(id) ON DELETE CASCADE,
  verdict       TEXT NOT NULL,
  score         INTEGER NOT NULL DEFAULT 0,
  max_score     INTEGER NOT NULL DEFAULT 0,
  time_ms       INTEGER NOT NULL DEFAULT 0,
  memory_kb     INTEGER NOT NULL DEFAULT 0,
  compile_log   TEXT NOT NULL DEFAULT '',
  detail_json   TEXT NOT NULL DEFAULT '[]',
  judged_at     INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS events (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id   INTEGER,
  exam_id      INTEGER,
  student_no   TEXT NOT NULL DEFAULT '',
  type         TEXT NOT NULL,
  severity     TEXT NOT NULL DEFAULT 'info',
  payload_json TEXT NOT NULL DEFAULT '{}',
  client_time  INTEGER NOT NULL DEFAULT 0,
  server_time  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_exam ON events(exam_id, id DESC);

CREATE TABLE IF NOT EXISTS audit_logs (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  actor        TEXT NOT NULL DEFAULT 'teacher',
  action       TEXT NOT NULL,
  target       TEXT NOT NULL DEFAULT '',
  payload_json TEXT NOT NULL DEFAULT '{}',
  created_at   INTEGER NOT NULL
);

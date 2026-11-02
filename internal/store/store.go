// Package store owns the SQLite file that holds the execution journal, the
// traceability graph, the audit chain, installed skills and eval evidence.
//
// One file per workspace (.gpn/gpn.db), WAL mode, one connection so writes
// serialize without a separate queue. The pure-Go driver keeps the binary
// static.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
)

// Store wraps the database.
type Store struct {
	DB   *sql.DB
	Path string
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{DB: db, Path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) migrate() error {
	_, err := s.DB.Exec(schema)
	return err
}

// Run is one workflow execution.
type Run struct {
	ID               string
	Workflow         string
	WorkflowHash     string
	PolicyHash       string
	ConstitutionHash string
	Status           string // running | waiting_approval | blocked | done | failed | stopped
	InputsJSON       string
	StartedAt        time.Time
	FinishedAt       *time.Time
	CostUSD          float64
	VendorCostUSD    float64
}

// CreateRun inserts a run.
func (s *Store) CreateRun(r Run) error {
	_, err := s.DB.Exec(`INSERT INTO runs(id, workflow, workflow_hash, policy_hash, constitution_hash, status, inputs_json, started_at) VALUES(?,?,?,?,?,?,?,?)`,
		r.ID, r.Workflow, r.WorkflowHash, r.PolicyHash, r.ConstitutionHash, r.Status, r.InputsJSON, r.StartedAt.UTC().Format(time.RFC3339Nano))
	return err
}

// GetRun loads a run.
func (s *Store) GetRun(id string) (Run, error) {
	var r Run
	var started string
	var finished sql.NullString
	err := s.DB.QueryRow(`SELECT id, workflow, workflow_hash, policy_hash, constitution_hash, status, inputs_json, started_at, finished_at, cost_usd, vendor_cost_usd FROM runs WHERE id=?`, id).
		Scan(&r.ID, &r.Workflow, &r.WorkflowHash, &r.PolicyHash, &r.ConstitutionHash, &r.Status, &r.InputsJSON, &started, &finished, &r.CostUSD, &r.VendorCostUSD)
	if err != nil {
		return r, err
	}
	r.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
	if finished.Valid {
		t, _ := time.Parse(time.RFC3339Nano, finished.String)
		r.FinishedAt = &t
	}
	return r, nil
}

// ListRuns returns the most recent runs.
func (s *Store) ListRuns(limit int) ([]Run, error) {
	rows, err := s.DB.Query(`SELECT id FROM runs ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	var out []Run
	for _, id := range ids {
		r, err := s.GetRun(id)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// SetRunStatus updates status and, for terminal states, finished_at.
func (s *Store) SetRunStatus(id, status string) error {
	switch status {
	case "done", "failed", "stopped":
		_, err := s.DB.Exec(`UPDATE runs SET status=?, finished_at=? WHERE id=?`, status, time.Now().UTC().Format(time.RFC3339Nano), id)
		return err
	}
	_, err := s.DB.Exec(`UPDATE runs SET status=? WHERE id=?`, status, id)
	return err
}

// AddRunCost accumulates cost.
func (s *Store) AddRunCost(id string, usd, vendorUSD float64) error {
	_, err := s.DB.Exec(`UPDATE runs SET cost_usd = cost_usd + ?, vendor_cost_usd = vendor_cost_usd + ? WHERE id=?`, usd, vendorUSD, id)
	return err
}

// Step is one journaled step attempt.
type Step struct {
	ID             int64
	RunID          string
	StepID         string
	Kind           string
	Attempt        int
	Status         string // pending | running | waiting_approval | done | blocked | failed
	Tier           string
	Route          string
	ModelRequested string
	ModelResponded string
	SessionID      string
	Worktree       string
	InputHash      string
	OutputJSON     string
	CostUSD        float64
	VendorCostUSD  float64
	SettingsHash   string
	SkillVersions  string
	Error          string
	StartedAt      time.Time
	FinishedAt     *time.Time
}

// BeginStep creates a new attempt row and returns it.
func (s *Store) BeginStep(runID, stepID, kind string) (Step, error) {
	var attempt int
	if err := s.DB.QueryRow(`SELECT COALESCE(MAX(attempt),0)+1 FROM steps WHERE run_id=? AND step_id=?`, runID, stepID).Scan(&attempt); err != nil {
		return Step{}, err
	}
	st := Step{RunID: runID, StepID: stepID, Kind: kind, Attempt: attempt, Status: "running", StartedAt: time.Now()}
	res, err := s.DB.Exec(`INSERT INTO steps(run_id, step_id, kind, attempt, status, started_at) VALUES(?,?,?,?,?,?)`,
		runID, stepID, kind, attempt, st.Status, st.StartedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return Step{}, err
	}
	st.ID, _ = res.LastInsertId()
	return st, nil
}

// UpdateStep writes the mutable fields of a step row.
func (s *Store) UpdateStep(st Step) error {
	var finished any
	if st.FinishedAt != nil {
		finished = st.FinishedAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := s.DB.Exec(`UPDATE steps SET status=?, tier=?, route=?, model_requested=?, model_responded=?, session_id=?, worktree=?, input_hash=?, output_json=?, cost_usd=?, vendor_cost_usd=?, settings_hash=?, skill_versions=?, error=?, finished_at=? WHERE id=?`,
		st.Status, st.Tier, st.Route, st.ModelRequested, st.ModelResponded, st.SessionID, st.Worktree, st.InputHash, st.OutputJSON, st.CostUSD, st.VendorCostUSD, st.SettingsHash, st.SkillVersions, st.Error, finished, st.ID)
	return err
}

// LatestStep returns the newest attempt of a step, if any.
func (s *Store) LatestStep(runID, stepID string) (Step, bool, error) {
	st, err := scanStep(s.DB.QueryRow(`SELECT `+stepCols+` FROM steps WHERE run_id=? AND step_id=? ORDER BY attempt DESC LIMIT 1`, runID, stepID))
	if errors.Is(err, sql.ErrNoRows) {
		return Step{}, false, nil
	}
	return st, err == nil, err
}

// ListSteps returns the newest attempt of every step in a run, in start order.
func (s *Store) ListSteps(runID string) ([]Step, error) {
	rows, err := s.DB.Query(`SELECT `+stepCols+` FROM steps s WHERE run_id=? AND attempt=(SELECT MAX(attempt) FROM steps s2 WHERE s2.run_id=s.run_id AND s2.step_id=s.step_id) ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Step
	for rows.Next() {
		st, err := scanStep(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

const stepCols = `id, run_id, step_id, kind, attempt, status, tier, route, model_requested, model_responded, session_id, worktree, input_hash, output_json, cost_usd, vendor_cost_usd, settings_hash, skill_versions, error, started_at, finished_at`

type scanner interface{ Scan(dest ...any) error }

func scanStep(sc scanner) (Step, error) {
	var st Step
	var started string
	var finished sql.NullString
	err := sc.Scan(&st.ID, &st.RunID, &st.StepID, &st.Kind, &st.Attempt, &st.Status, &st.Tier, &st.Route, &st.ModelRequested, &st.ModelResponded, &st.SessionID, &st.Worktree, &st.InputHash, &st.OutputJSON, &st.CostUSD, &st.VendorCostUSD, &st.SettingsHash, &st.SkillVersions, &st.Error, &started, &finished)
	if err != nil {
		return st, err
	}
	st.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
	if finished.Valid {
		t, _ := time.Parse(time.RFC3339Nano, finished.String)
		st.FinishedAt = &t
	}
	return st, nil
}

// Event is one normalized driver event persisted for audit and lineage.
type Event struct {
	StepRowID  int64
	Seq        int
	Type       string
	ToolName   string
	ToolUseID  string
	InputHash  string
	OutputHash string
	Payload    string
	Time       time.Time
}

// InsertEvent appends an event.
func (s *Store) InsertEvent(e Event) error {
	_, err := s.DB.Exec(`INSERT INTO events(step_row_id, seq, type, tool_name, tool_use_id, input_hash, output_hash, payload_json, ts) VALUES(?,?,?,?,?,?,?,?,?)`,
		e.StepRowID, e.Seq, e.Type, e.ToolName, e.ToolUseID, e.InputHash, e.OutputHash, e.Payload, e.Time.UTC().Format(time.RFC3339Nano))
	return err
}

// CountEvents returns the number of events for a step row.
func (s *Store) CountEvents(stepRowID int64) (int, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM events WHERE step_row_id=?`, stepRowID).Scan(&n)
	return n, err
}

// Approval is a durable human decision request.
type Approval struct {
	ID           int64
	RunID        string
	StepID       string
	Class        string
	ToolCallJSON string
	RequestedAt  time.Time
	DecidedAt    *time.Time
	Decider      string
	Decision     string // "" (pending) | approve | deny
	Reason       string
}

// RequestApproval creates a pending approval unless an identical pending one exists.
func (s *Store) RequestApproval(a Approval) (Approval, error) {
	existing, ok, err := s.PendingApproval(a.RunID, a.StepID, a.Class)
	if err != nil {
		return Approval{}, err
	}
	if ok {
		return existing, nil
	}
	a.RequestedAt = time.Now()
	res, err := s.DB.Exec(`INSERT INTO approvals(run_id, step_id, class, tool_call_json, requested_at, decision) VALUES(?,?,?,?,?,'')`, a.RunID, a.StepID, a.Class, a.ToolCallJSON, a.RequestedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return Approval{}, err
	}
	a.ID, _ = res.LastInsertId()
	return a, nil
}

// PendingApproval finds an undecided approval.
func (s *Store) PendingApproval(runID, stepID, class string) (Approval, bool, error) {
	a, err := scanApproval(s.DB.QueryRow(`SELECT `+approvalCols+` FROM approvals WHERE run_id=? AND step_id=? AND class=? AND decision='' ORDER BY id DESC LIMIT 1`, runID, stepID, class))
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, false, nil
	}
	return a, err == nil, err
}

// GetApproval loads one approval by id.
func (s *Store) GetApproval(id int64) (Approval, error) {
	return scanApproval(s.DB.QueryRow(`SELECT `+approvalCols+` FROM approvals WHERE id=?`, id))
}

// LatestApproval returns the newest approval for a run step regardless of state.
func (s *Store) LatestApproval(runID, stepID string) (Approval, bool, error) {
	a, err := scanApproval(s.DB.QueryRow(`SELECT `+approvalCols+` FROM approvals WHERE run_id=? AND step_id=? ORDER BY id DESC LIMIT 1`, runID, stepID))
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, false, nil
	}
	return a, err == nil, err
}

// ListPendingApprovals lists undecided approvals, optionally for one run.
func (s *Store) ListPendingApprovals(runID string) ([]Approval, error) {
	q := `SELECT ` + approvalCols + ` FROM approvals WHERE decision=''`
	var args []any
	if runID != "" {
		q += ` AND run_id=?`
		args = append(args, runID)
	}
	rows, err := s.DB.Query(q+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DecideApproval records a decision.
func (s *Store) DecideApproval(id int64, decider, decision, reason string) error {
	if decision != "approve" && decision != "deny" {
		return fmt.Errorf("decision must be approve or deny")
	}
	res, err := s.DB.Exec(`UPDATE approvals SET decided_at=?, decider=?, decision=?, reason=? WHERE id=? AND decision=''`, time.Now().UTC().Format(time.RFC3339Nano), decider, decision, reason, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("approval %d not found or already decided", id)
	}
	return nil
}

const approvalCols = `id, run_id, step_id, class, tool_call_json, requested_at, decided_at, decider, decision, reason`

func scanApproval(sc scanner) (Approval, error) {
	var a Approval
	var requested string
	var decided sql.NullString
	err := sc.Scan(&a.ID, &a.RunID, &a.StepID, &a.Class, &a.ToolCallJSON, &requested, &decided, &a.Decider, &a.Decision, &a.Reason)
	if err != nil {
		return a, err
	}
	a.RequestedAt, _ = time.Parse(time.RFC3339Nano, requested)
	if decided.Valid {
		t, _ := time.Parse(time.RFC3339Nano, decided.String)
		a.DecidedAt = &t
	}
	return a, nil
}

// Node is a traceability graph vertex.
type Node struct {
	ID    string
	Type  string
	Key   string
	Props map[string]any
}

// UpsertNode inserts or updates a node.
func (s *Store) UpsertNode(n Node) error {
	props, _ := json.Marshal(n.Props)
	if n.Props == nil {
		props = []byte("{}")
	}
	_, err := s.DB.Exec(`INSERT INTO nodes(id, type, key, props_json, created_at) VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET type=excluded.type, key=excluded.key, props_json=excluded.props_json`,
		n.ID, n.Type, n.Key, string(props), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// Edge is a traceability graph edge.
type Edge struct {
	Src   string
	Dst   string
	Type  string
	Props map[string]any
}

// InsertEdge adds an edge (idempotent on src, dst, type).
func (s *Store) InsertEdge(e Edge) error {
	props, _ := json.Marshal(e.Props)
	if e.Props == nil {
		props = []byte("{}")
	}
	_, err := s.DB.Exec(`INSERT OR IGNORE INTO edges(src, dst, type, props_json, created_at) VALUES(?,?,?,?,?)`,
		e.Src, e.Dst, e.Type, string(props), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// AuditRow is one hash-chained audit entry.
type AuditRow struct {
	Seq         int64
	Time        time.Time
	RunID       string
	StepID      string
	Kind        string
	PayloadJSON string
	PayloadHash string
	PrevHash    string
	Hash        string
}

// LastAudit returns the newest audit row (seq 0 and empty hash when none).
func (s *Store) LastAudit() (AuditRow, error) {
	var a AuditRow
	var ts string
	err := s.DB.QueryRow(`SELECT seq, ts, run_id, step_id, kind, payload_json, payload_hash, prev_hash, hash FROM audit ORDER BY seq DESC LIMIT 1`).
		Scan(&a.Seq, &ts, &a.RunID, &a.StepID, &a.Kind, &a.PayloadJSON, &a.PayloadHash, &a.PrevHash, &a.Hash)
	if errors.Is(err, sql.ErrNoRows) {
		return AuditRow{}, nil
	}
	if err != nil {
		return a, err
	}
	a.Time, _ = time.Parse(time.RFC3339Nano, ts)
	return a, nil
}

// InsertAudit appends an audit row.
func (s *Store) InsertAudit(a AuditRow) error {
	_, err := s.DB.Exec(`INSERT INTO audit(seq, ts, run_id, step_id, kind, payload_json, payload_hash, prev_hash, hash) VALUES(?,?,?,?,?,?,?,?,?)`,
		a.Seq, a.Time.UTC().Format(time.RFC3339Nano), a.RunID, a.StepID, a.Kind, a.PayloadJSON, a.PayloadHash, a.PrevHash, a.Hash)
	return err
}

// ListAudit returns all audit rows in order.
func (s *Store) ListAudit() ([]AuditRow, error) {
	rows, err := s.DB.Query(`SELECT seq, ts, run_id, step_id, kind, payload_json, payload_hash, prev_hash, hash FROM audit ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditRow
	for rows.Next() {
		var a AuditRow
		var ts string
		if err := rows.Scan(&a.Seq, &ts, &a.RunID, &a.StepID, &a.Kind, &a.PayloadJSON, &a.PayloadHash, &a.PrevHash, &a.Hash); err != nil {
			return nil, err
		}
		a.Time, _ = time.Parse(time.RFC3339Nano, ts)
		out = append(out, a)
	}
	return out, rows.Err()
}

// Skill is an installed registry entry.
type Skill struct {
	Name         string
	Version      string
	Tier         string
	Digest       string
	Signature    string
	Owner        string
	Path         string
	EvalReportID string
	InstalledAt  time.Time
}

// UpsertSkill records an install.
func (s *Store) UpsertSkill(sk Skill) error {
	_, err := s.DB.Exec(`INSERT INTO skills(name, version, tier, digest, signature, owner, path, eval_report_id, installed_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(name, version) DO UPDATE SET tier=excluded.tier, digest=excluded.digest, signature=excluded.signature, owner=excluded.owner, path=excluded.path, eval_report_id=excluded.eval_report_id, installed_at=excluded.installed_at`,
		sk.Name, sk.Version, sk.Tier, sk.Digest, sk.Signature, sk.Owner, sk.Path, sk.EvalReportID, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// ListSkills lists installed skills.
func (s *Store) ListSkills() ([]Skill, error) {
	rows, err := s.DB.Query(`SELECT name, version, tier, digest, signature, owner, path, eval_report_id, installed_at FROM skills ORDER BY name, version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Skill
	for rows.Next() {
		var sk Skill
		var ts string
		if err := rows.Scan(&sk.Name, &sk.Version, &sk.Tier, &sk.Digest, &sk.Signature, &sk.Owner, &sk.Path, &sk.EvalReportID, &ts); err != nil {
			return nil, err
		}
		sk.InstalledAt, _ = time.Parse(time.RFC3339Nano, ts)
		out = append(out, sk)
	}
	return out, rows.Err()
}

// EvalReport is an archived eval run.
type EvalReport struct {
	ID          string
	Skill       string
	Version     string
	JudgeModel  string
	Runs        int
	PassRate    float64
	Threshold   float64
	CostUSD     float64
	ResultsJSON string
}

// InsertEvalReport archives an eval report.
func (s *Store) InsertEvalReport(r EvalReport) error {
	_, err := s.DB.Exec(`INSERT INTO eval_reports(id, skill, version, ran_at, judge_model, runs, pass_rate, threshold, cost_usd, results_json) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Skill, r.Version, time.Now().UTC().Format(time.RFC3339Nano), r.JudgeModel, r.Runs, r.PassRate, r.Threshold, r.CostUSD, r.ResultsJSON)
	return err
}

const schema = `
CREATE TABLE IF NOT EXISTS runs (
  id TEXT PRIMARY KEY,
  workflow TEXT NOT NULL,
  workflow_hash TEXT NOT NULL DEFAULT '',
  policy_hash TEXT NOT NULL DEFAULT '',
  constitution_hash TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  inputs_json TEXT NOT NULL DEFAULT '{}',
  started_at TEXT NOT NULL,
  finished_at TEXT,
  cost_usd REAL NOT NULL DEFAULT 0,
  vendor_cost_usd REAL NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS steps (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id TEXT NOT NULL REFERENCES runs(id),
  step_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  attempt INTEGER NOT NULL,
  status TEXT NOT NULL,
  tier TEXT NOT NULL DEFAULT '',
  route TEXT NOT NULL DEFAULT '',
  model_requested TEXT NOT NULL DEFAULT '',
  model_responded TEXT NOT NULL DEFAULT '',
  session_id TEXT NOT NULL DEFAULT '',
  worktree TEXT NOT NULL DEFAULT '',
  input_hash TEXT NOT NULL DEFAULT '',
  output_json TEXT NOT NULL DEFAULT '',
  cost_usd REAL NOT NULL DEFAULT 0,
  vendor_cost_usd REAL NOT NULL DEFAULT 0,
  settings_hash TEXT NOT NULL DEFAULT '',
  skill_versions TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL,
  finished_at TEXT,
  UNIQUE(run_id, step_id, attempt)
);
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  step_row_id INTEGER NOT NULL REFERENCES steps(id),
  seq INTEGER NOT NULL,
  type TEXT NOT NULL,
  tool_name TEXT NOT NULL DEFAULT '',
  tool_use_id TEXT NOT NULL DEFAULT '',
  input_hash TEXT NOT NULL DEFAULT '',
  output_hash TEXT NOT NULL DEFAULT '',
  payload_json TEXT NOT NULL DEFAULT '',
  ts TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_step ON events(step_row_id, seq);
CREATE TABLE IF NOT EXISTS approvals (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id TEXT NOT NULL,
  step_id TEXT NOT NULL,
  class TEXT NOT NULL,
  tool_call_json TEXT NOT NULL DEFAULT '',
  requested_at TEXT NOT NULL,
  decided_at TEXT,
  decider TEXT NOT NULL DEFAULT '',
  decision TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS nodes (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL,
  key TEXT NOT NULL DEFAULT '',
  props_json TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS nodes_type ON nodes(type);
CREATE TABLE IF NOT EXISTS edges (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  src TEXT NOT NULL,
  dst TEXT NOT NULL,
  type TEXT NOT NULL,
  props_json TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL,
  UNIQUE(src, dst, type)
);
CREATE INDEX IF NOT EXISTS edges_src ON edges(src, type);
CREATE INDEX IF NOT EXISTS edges_dst ON edges(dst, type);
CREATE TABLE IF NOT EXISTS audit (
  seq INTEGER PRIMARY KEY,
  ts TEXT NOT NULL,
  run_id TEXT NOT NULL DEFAULT '',
  step_id TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL,
  payload_json TEXT NOT NULL,
  payload_hash TEXT NOT NULL,
  prev_hash TEXT NOT NULL,
  hash TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS skills (
  name TEXT NOT NULL,
  version TEXT NOT NULL,
  tier TEXT NOT NULL,
  digest TEXT NOT NULL,
  signature TEXT NOT NULL DEFAULT '',
  owner TEXT NOT NULL DEFAULT '',
  path TEXT NOT NULL DEFAULT '',
  eval_report_id TEXT NOT NULL DEFAULT '',
  installed_at TEXT NOT NULL,
  PRIMARY KEY(name, version)
);
CREATE TABLE IF NOT EXISTS eval_reports (
  id TEXT PRIMARY KEY,
  skill TEXT NOT NULL,
  version TEXT NOT NULL,
  ran_at TEXT NOT NULL,
  judge_model TEXT NOT NULL DEFAULT '',
  runs INTEGER NOT NULL DEFAULT 0,
  pass_rate REAL NOT NULL DEFAULT 0,
  threshold REAL NOT NULL DEFAULT 0,
  cost_usd REAL NOT NULL DEFAULT 0,
  results_json TEXT NOT NULL DEFAULT ''
);

-- Standard traceability queries as views.
CREATE VIEW IF NOT EXISTS v_coverage_gap AS
  SELECT r.id AS requirement_id, r.key AS requirement_key
  FROM nodes r
  WHERE r.type = 'Requirement'
    AND NOT EXISTS (
      SELECT 1 FROM edges v
      JOIN nodes tr ON tr.id = v.dst AND tr.type = 'TestRun'
      WHERE v.src = r.id AND v.type = 'VERIFIED_BY'
        AND json_extract(tr.props_json, '$.status') = 'pass');

CREATE VIEW IF NOT EXISTS v_orphans AS
  SELECT f.id AS file_id, f.key AS path
  FROM nodes f
  WHERE f.type = 'File'
    AND NOT EXISTS (SELECT 1 FROM edges e WHERE e.dst = f.id AND e.type = 'IMPLEMENTED_IN');
`

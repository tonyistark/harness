// Package trace is the traceability graph: typed nodes and edges in SQLite,
// the five standard queries, and the "why did this line change" walk.
package trace

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gpn/gpn/internal/store"
)

// Node and edge type names. Kept as constants so every writer agrees.
const (
	Requirement = "Requirement"
	Decision    = "Decision"
	Risk        = "Risk"
	Task        = "Task"
	Test        = "Test"
	TestRun     = "TestRun"
	File        = "File"
	Symbol      = "Symbol"
	Commit      = "Commit"
	PR          = "PR"
	AgentRun    = "AgentRun"
	Skill       = "Skill"
	Model       = "Model"
	Approval    = "Approval"

	Refines       = "REFINES"
	SatisfiedBy   = "SATISFIED_BY"
	VerifiedBy    = "VERIFIED_BY"
	ImplementedIn = "IMPLEMENTED_IN"
	ChangedIn     = "CHANGED_IN"
	ExecutedBy    = "EXECUTED_BY"
	ApprovedBy    = "APPROVED_BY"
	DerivedFrom   = "DERIVED_FROM"
	DependsOn     = "DEPENDS_ON"
	Suspect       = "SUSPECT"
)

// Graph wraps the store with typed helpers.
type Graph struct{ S *store.Store }

// ID builds a stable node id from a type and key.
func ID(typ, key string) string { return typ + ":" + key }

// Put upserts a node.
func (g *Graph) Put(typ, key string, props map[string]any) (string, error) {
	id := ID(typ, key)
	return id, g.S.UpsertNode(store.Node{ID: id, Type: typ, Key: key, Props: props})
}

// Link adds an edge between two node ids.
func (g *Graph) Link(src, dst, typ string, props map[string]any) error {
	return g.S.InsertEdge(store.Edge{Src: src, Dst: dst, Type: typ, Props: props})
}

// Row is a generic query result row.
type Row map[string]any

// Query runs one of the standard queries by name.
func (g *Graph) Query(name string, args map[string]string) ([]Row, error) {
	switch name {
	case "coverage_gap":
		return g.rows(`SELECT requirement_id, requirement_key FROM v_coverage_gap ORDER BY requirement_key`)
	case "orphans":
		return g.rows(`SELECT file_id, path FROM v_orphans ORDER BY path`)
	case "impact":
		return g.impact(args["node"])
	case "blast_radius":
		return g.blastRadius(args["file"])
	case "why":
		line, _ := strconv.Atoi(args["line"])
		return g.Why(args["repo"], args["file"], line)
	}
	return nil, fmt.Errorf("unknown query %q (coverage_gap, orphans, impact, blast_radius, why)", name)
}

func (g *Graph) rows(q string, args ...any) ([]Row, error) {
	rs, err := g.S.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	return collect(rs)
}

func collect(rs *sql.Rows) ([]Row, error) {
	cols, err := rs.Columns()
	if err != nil {
		return nil, err
	}
	var out []Row
	for rs.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, err
		}
		r := Row{}
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				r[c] = string(b)
			} else {
				r[c] = vals[i]
			}
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

// impact walks downstream of a node over the derivation edges and marks the
// reached nodes SUSPECT (tenet: a spec change reopens dependents).
func (g *Graph) impact(nodeID string) ([]Row, error) {
	if nodeID == "" {
		return nil, fmt.Errorf("impact needs node=<id>")
	}
	rows, err := g.rows(`
WITH RECURSIVE down(id, depth) AS (
  SELECT ?, 0
  UNION
  SELECT e.dst, down.depth + 1 FROM edges e JOIN down ON e.src = down.id
   WHERE e.type IN ('REFINES','SATISFIED_BY','IMPLEMENTED_IN','VERIFIED_BY','DEPENDS_ON') AND down.depth < 12
)
SELECT n.id, n.type, n.key, down.depth FROM down JOIN nodes n ON n.id = down.id WHERE down.depth > 0 ORDER BY down.depth, n.type, n.key`, nodeID)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if id, ok := r["id"].(string); ok {
			_ = g.Link(nodeID, id, Suspect, map[string]any{"reason": "upstream change"})
		}
	}
	return rows, nil
}

// blastRadius lists requirements served by a file.
func (g *Graph) blastRadius(path string) ([]Row, error) {
	if path == "" {
		return nil, fmt.Errorf("blast_radius needs file=<path>")
	}
	return g.rows(`
WITH RECURSIVE up(id, depth) AS (
  SELECT ?, 0
  UNION
  SELECT e.src, up.depth + 1 FROM edges e JOIN up ON e.dst = up.id
   WHERE e.type IN ('IMPLEMENTED_IN','SATISFIED_BY','REFINES') AND up.depth < 8
)
SELECT n.id, n.type, n.key, up.depth FROM up JOIN nodes n ON n.id = up.id WHERE n.type IN ('Requirement','Task') ORDER BY n.type, n.key`, ID(File, path))
}

// WhyResult is the audit answer for one line.
type WhyResult struct {
	File          string            `json:"file"`
	Line          int               `json:"line"`
	Commit        string            `json:"commit"`
	Author        string            `json:"author"`
	Trailers      map[string]string `json:"trailers"`
	RunID         string            `json:"run_id"`
	StepID        string            `json:"step_id"`
	TaskID        string            `json:"task_id"`
	Requirements  []string          `json:"requirements"`
	Model         string            `json:"model"`
	SkillVersions string            `json:"skill_versions"`
	Approver      string            `json:"approver"`
	CostUSD       float64           `json:"cost_usd"`
}

// Why resolves a file:line to the commit, task, requirement, agent run,
// model, skill versions, approver and cost that produced it.
func (g *Graph) Why(repo, file string, line int) ([]Row, error) {
	if file == "" || line <= 0 {
		return nil, fmt.Errorf("why needs file=<path> line=<n>")
	}
	res := WhyResult{File: file, Line: line, Trailers: map[string]string{}}
	if _, err := os.Stat(filepath.Join(repo, file)); err != nil {
		return nil, fmt.Errorf("why: %s does not exist in %s", file, repo)
	}
	out, err := exec.Command("git", "-C", repo, "blame", "-L", fmt.Sprintf("%d,%d", line, line), "--porcelain", file).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			msg := strings.TrimSpace(string(ee.Stderr))
			if msg == "" {
				msg = "is the file committed, and is the line number within the file?"
			}
			return nil, fmt.Errorf("why: git blame %s:%d failed: %s", file, line, msg)
		}
		return nil, fmt.Errorf("why: git blame: %w", err)
	}
	lines := strings.Split(string(out), "\n")
	if len(lines) > 0 {
		res.Commit = strings.Fields(lines[0])[0]
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "author ") {
			res.Author = strings.TrimPrefix(l, "author ")
		}
	}
	msg, err := exec.Command("git", "-C", repo, "log", "-1", "--format=%B", res.Commit).Output()
	if err == nil {
		res.Trailers = Trailers(string(msg))
	}
	res.RunID = res.Trailers["GPN-Run"]
	res.StepID = res.Trailers["GPN-Step"]
	res.TaskID = res.Trailers["GPN-Task"]
	if reqs := res.Trailers["GPN-Req"]; reqs != "" {
		for _, r := range strings.Split(reqs, ",") {
			res.Requirements = append(res.Requirements, strings.TrimSpace(r))
		}
	}
	if res.RunID != "" && res.StepID != "" {
		if st, ok, _ := g.S.LatestStep(res.RunID, res.StepID); ok {
			res.Model = st.ModelResponded
			res.SkillVersions = st.SkillVersions
			res.CostUSD = st.CostUSD
		}
		if a, ok, _ := g.S.LatestApproval(res.RunID, res.StepID); ok && a.Decision == "approve" {
			res.Approver = a.Decider
		} else {
			// No approval on the step itself: report the run's latest human
			// approval (e.g. the requirements sign-off) and which step it was.
			var decider, step string
			if err := g.S.DB.QueryRow(`SELECT decider, step_id FROM approvals WHERE run_id=? AND decision='approve' ORDER BY id DESC LIMIT 1`, res.RunID).Scan(&decider, &step); err == nil {
				res.Approver = decider + " (" + step + ")"
			}
		}
	}
	b, _ := json.Marshal(res)
	var r Row
	_ = json.Unmarshal(b, &r)
	return []Row{r}, nil
}

// Trailers parses git trailers ("Key: value" lines at the end of a message).
func Trailers(msg string) map[string]string {
	out := map[string]string{}
	for _, l := range strings.Split(msg, "\n") {
		l = strings.TrimSpace(l)
		i := strings.Index(l, ": ")
		if i <= 0 || strings.ContainsAny(l[:i], " \t") {
			continue
		}
		key := l[:i]
		if strings.HasPrefix(key, "GPN-") {
			out[key] = strings.TrimSpace(l[i+2:])
		}
	}
	return out
}

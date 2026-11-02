package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gpn/gpn/internal/assets"
	"github.com/gpn/gpn/internal/audit"
	"github.com/gpn/gpn/internal/driver/mock"
	"github.com/gpn/gpn/internal/policy"
	"github.com/gpn/gpn/internal/registry"
	"github.com/gpn/gpn/internal/store"
	"github.com/gpn/gpn/internal/trace"
	"github.com/gpn/gpn/internal/vcs"
	"github.com/gpn/gpn/internal/workflow"
)

const testPolicy = `
version: 1
autonomy: supervised
sandbox: { required: true }
protected_paths: [".git/hooks/**", ".gpn/**"]
commands:
  deny: ["git push --force*", "rm -rf *"]
  approve: ["git push*"]
tools:
  allow: [Read, Edit, Write]
  deny: [WebFetch]
models:
  catalog: .gpn/model-catalog.json
  route: bedrock
  tiers:
    opus:   { catalog: opus,   fallback: sonnet }
    sonnet: { catalog: sonnet, fallback: haiku }
    haiku:  { catalog: haiku }
  classes: { design: opus, review: opus, implement: sonnet, mechanical: haiku }
budgets: { per_step_usd: 2, per_run_usd: 25, alert_at: 0.8 }
registry: { tiers_allowed: [official], require_signature: true }
gates:
  build: "true"
  test: "true"
  coverage: "echo 'total: (statements) 85.0%'"
  sast: "true"
  sca: "true"
`

const testCatalog = `{"models":{
 "opus":   {"route":{"bedrock":"b-opus","anthropic":"a-opus"},"price_in_per_mtok":15,"price_out_per_mtok":75,"cleared_classes":["*"]},
 "sonnet": {"route":{"bedrock":"b-sonnet","anthropic":"a-sonnet"},"price_in_per_mtok":3,"price_out_per_mtok":15,"cleared_classes":["*"]},
 "haiku":  {"route":{"bedrock":"b-haiku","anthropic":"a-haiku"},"price_in_per_mtok":0.8,"price_out_per_mtok":4,"cleared_classes":["*"]}}}`

type fixture struct {
	root   string
	st     *store.Store
	p      *policy.Policy
	cat    *policy.Catalog
	wf     *workflow.Workflow
	wfSrc  []byte
	out    bytes.Buffer
	driver *mock.Driver
}

func setup(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	if err := vcs.Init(root); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, ".gpn"), 0o755))
	must(os.WriteFile(filepath.Join(root, ".gpn", ".gitignore"), []byte("*\n"), 0o644))
	must(os.WriteFile(filepath.Join(root, "gpn-policy.yaml"), []byte(testPolicy), 0o644))
	must(os.WriteFile(filepath.Join(root, ".gpn", "model-catalog.json"), []byte(testCatalog), 0o644))
	must(assets.Extract("workflows", filepath.Join(root, "workflows"), true))
	must(assets.Extract("schemas", filepath.Join(root, "schemas"), true))
	must(assets.Extract("prompts", filepath.Join(root, "prompts"), true))
	must(assets.Extract("registry/sdd-core", filepath.Join(root, ".gpn", "registry", "sdd-core"), true))
	// Commit the scaffold so worktrees branch from a clean base.
	if _, err := (vcs.Repo{Dir: root}).Commit("scaffold", nil); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(root, ".gpn", "gpn.db"))
	must(err)
	t.Cleanup(func() { st.Close() })
	p, err := policy.Load(filepath.Join(root, "gpn-policy.yaml"))
	must(err)
	cat, err := p.LoadCatalog(root)
	must(err)
	keys, err := registry.GenerateKeys(filepath.Join(root, ".gpn", "keys"), "test")
	must(err)
	plugin := filepath.Join(root, ".gpn", "registry", "sdd-core")
	_, _, err = registry.Publish(plugin, keys, "test")
	must(err)
	_, err = registry.Install(st, plugin, keys, p.Registry.TiersAllowed, true)
	must(err)
	src, err := os.ReadFile(filepath.Join(root, "workflows", "feature.yaml"))
	must(err)
	wf, err := workflow.Parse(src)
	must(err)
	return &fixture{root: root, st: st, p: p, cat: cat, wf: wf, wfSrc: src, driver: mock.New()}
}

func (f *fixture) engine(t *testing.T, route string, overrides map[string]string) *Engine {
	t.Helper()
	e, err := New(Config{Root: f.root, PolicyPath: filepath.Join(f.root, "gpn-policy.yaml"), Policy: f.p, Catalog: f.cat, Store: f.st, Driver: f.driver, Out: &f.out, GpnBinary: "/usr/local/bin/gpn", RouteOverride: route, ClassOverrides: overrides})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestFeatureWorkflowEndToEnd(t *testing.T) {
	f := setup(t)
	e := f.engine(t, "", nil)
	ctx := context.Background()

	runID, err := e.Run(ctx, f.wf, f.wfSrc, map[string]string{"jira": "GPN-123"})
	if !errors.Is(err, ErrWaitingApproval) {
		t.Fatalf("expected waiting for approval, got %v\n%s", err, f.out.String())
	}
	run, _ := f.st.GetRun(runID)
	if run.Status != "waiting_approval" {
		t.Fatalf("run status %s", run.Status)
	}
	pend, _ := f.st.ListPendingApprovals(runID)
	if len(pend) != 1 || pend[0].StepID != "approve_requirements" {
		t.Fatalf("pending approvals %+v", pend)
	}
	// Resume before deciding: still waiting, intake not re-run.
	if err := e.Resume(ctx, runID, f.wf); !errors.Is(err, ErrWaitingApproval) {
		t.Fatalf("resume before approval: %v", err)
	}
	steps, _ := f.st.ListSteps(runID)
	if n := countStep(steps, "intake"); n != 1 {
		t.Fatalf("intake ran %d times before approval", n)
	}
	if st, ok, _ := f.st.LatestStep(runID, "intake"); !ok || st.Attempt != 1 || st.ModelRequested != "b-opus" || st.Tier != "opus" {
		t.Fatalf("intake step %+v", st)
	}

	if err := f.st.DecideApproval(pend[0].ID, "tony", "approve", "scope confirmed"); err != nil {
		t.Fatal(err)
	}
	if err := e.Resume(ctx, runID, f.wf); err != nil {
		t.Fatalf("resume after approval: %v\n%s", err, f.out.String())
	}
	run, _ = f.st.GetRun(runID)
	if run.Status != "done" {
		t.Fatalf("run status %s\n%s", run.Status, f.out.String())
	}
	if run.CostUSD <= 0 || run.VendorCostUSD <= 0 {
		t.Fatalf("cost not recorded: %+v", run)
	}
	steps, _ = f.st.ListSteps(runID)
	for _, want := range []string{"intake", "approve_requirements", "design", "tasks", "implement[T1]", "implement[T2]", "implement[T3]", "implement", "review[T1]", "review", "gates", "merge", "pr"} {
		if st := find(steps, want); st == nil || st.Status != "done" {
			t.Errorf("step %s missing or not done: %+v", want, st)
		}
	}
	if st := find(steps, "implement[T3]"); st != nil && st.Tier != "haiku" {
		t.Errorf("mechanical task should route to haiku, got %s", st.Tier)
	}
	// Worktrees merged: the implementation files are on main with lineage trailers.
	for _, name := range []string{"implement[T1].md", "implement[T2].md", "implement[T3].md"} {
		if _, err := os.Stat(filepath.Join(f.root, "specs", name)); err != nil {
			t.Errorf("merged file missing: %s", name)
		}
	}
	g := &trace.Graph{S: f.st}
	rows, err := g.Query("why", map[string]string{"repo": f.root, "file": "specs/implement[T1].md", "line": "1"})
	if err != nil {
		t.Fatalf("why: %v", err)
	}
	if rows[0]["run_id"] != runID || rows[0]["task_id"] != "T1" {
		t.Fatalf("why answer %v", rows[0])
	}
	if reqs, _ := rows[0]["requirements"].([]any); len(reqs) != 1 || reqs[0] != "REQ-001" {
		t.Fatalf("why requirements %v", rows[0]["requirements"])
	}
	gap, err := g.Query("coverage_gap", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(gap) != 0 {
		t.Errorf("gates passed, expected no coverage gap, got %v", gap)
	}
	blast, _ := g.Query("blast_radius", map[string]string{"file": "internal/ingest/ingest.go"})
	if len(blast) == 0 {
		t.Errorf("blast radius empty for a task file")
	}
	if n, err := (&audit.Chain{S: f.st}).Verify(); err != nil || n < 10 {
		t.Fatalf("audit verify: %d %v", n, err)
	}
	if !strings.Contains(f.out.String(), "approved by tony") {
		t.Errorf("console output missing approval line:\n%s", f.out.String())
	}
}

func TestRouteAndTierOverride(t *testing.T) {
	f := setup(t)
	e := f.engine(t, "anthropic", map[string]string{"implement": "haiku"})
	runID, err := e.Run(context.Background(), f.wf, f.wfSrc, map[string]string{"jira": "GPN-7"})
	if !errors.Is(err, ErrWaitingApproval) {
		t.Fatalf("expected waiting, got %v", err)
	}
	st, _, _ := f.st.LatestStep(runID, "intake")
	if st.Route != "anthropic" || st.ModelRequested != "a-opus" {
		t.Fatalf("route override not applied: %+v", st)
	}
	pend, _ := f.st.ListPendingApprovals(runID)
	f.st.DecideApproval(pend[0].ID, "tony", "approve", "")
	if err := e.Resume(context.Background(), runID, f.wf); err != nil {
		t.Fatalf("resume: %v\n%s", err, f.out.String())
	}
	impl, _, _ := f.st.LatestStep(runID, "implement[T1]")
	if impl.Tier != "haiku" || impl.ModelRequested != "a-haiku" {
		t.Fatalf("class override implement=haiku not applied: %+v", impl)
	}
}

func TestGateFailureBlocksMerge(t *testing.T) {
	f := setup(t)
	f.p.Gates["coverage"] = "echo 'total: (statements) 42.0%'"
	e := f.engine(t, "", nil)
	runID, _ := e.Run(context.Background(), f.wf, f.wfSrc, map[string]string{"jira": "GPN-9"})
	pend, _ := f.st.ListPendingApprovals(runID)
	f.st.DecideApproval(pend[0].ID, "tony", "approve", "")
	err := e.Resume(context.Background(), runID, f.wf)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("expected gate block, got %v", err)
	}
	if st, _, _ := f.st.LatestStep(runID, "merge"); st.ID != 0 {
		t.Fatalf("merge ran despite a red gate")
	}
	run, _ := f.st.GetRun(runID)
	if run.Status != "blocked" {
		t.Fatalf("run status %s", run.Status)
	}
}

func TestDenyApprovalFailsRun(t *testing.T) {
	f := setup(t)
	e := f.engine(t, "", nil)
	runID, _ := e.Run(context.Background(), f.wf, f.wfSrc, map[string]string{"jira": "GPN-11"})
	pend, _ := f.st.ListPendingApprovals(runID)
	f.st.DecideApproval(pend[0].ID, "po", "deny", "out of scope")
	err := e.Resume(context.Background(), runID, f.wf)
	if err == nil || !strings.Contains(err.Error(), "denied by po") {
		t.Fatalf("expected denial, got %v", err)
	}
}

func TestTemplating(t *testing.T) {
	vars := map[string]any{"run": map[string]any{"id": "RUN-1"}, "item": map[string]any{"class": "mechanical"}, "steps": map[string]any{"tasks": map[string]any{"output": map[string]any{"tasks": []any{"a"}}}}}
	if got := render("gpn internal merge --run ${run.id}", vars); got != "gpn internal merge --run RUN-1" {
		t.Fatal(got)
	}
	if got := render("${item.class | implement}", vars); got != "mechanical" {
		t.Fatal(got)
	}
	if got := render("${item.missing | implement}", vars); got != "implement" {
		t.Fatal(got)
	}
	if l, ok := eval("${steps.tasks.output.tasks}", vars).([]any); !ok || len(l) != 1 {
		t.Fatal("for_each did not resolve to a list")
	}
}

func countStep(steps []store.Step, id string) int {
	n := 0
	for _, s := range steps {
		if s.StepID == id {
			n += s.Attempt
		}
	}
	return n
}

func find(steps []store.Step, id string) *store.Step {
	for i := range steps {
		if steps[i].StepID == id {
			return &steps[i]
		}
	}
	return nil
}

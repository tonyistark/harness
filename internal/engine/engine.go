// Package engine executes workflows as journaled steps: it resolves models
// from the policy, drives Claude Code through the driver, records events,
// lineage, cost and audit entries, pauses for approvals, runs gates, and
// resumes from the journal after a crash.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gpn/gpn/internal/audit"
	"github.com/gpn/gpn/internal/cost"
	"github.com/gpn/gpn/internal/driver"
	"github.com/gpn/gpn/internal/gates"
	"github.com/gpn/gpn/internal/policy"
	"github.com/gpn/gpn/internal/store"
	"github.com/gpn/gpn/internal/telemetry"
	"github.com/gpn/gpn/internal/trace"
	"github.com/gpn/gpn/internal/vcs"
	"github.com/gpn/gpn/internal/workflow"
)

// Sentinel errors the CLI maps to exit codes and messages.
var (
	ErrWaitingApproval = errors.New("run is waiting for an approval")
	ErrBlocked         = errors.New("run is blocked by a failed gate")
	ErrBudget          = errors.New("run budget exhausted")
	ErrStopped         = errors.New("run was stopped")
)

// Config wires an engine.
type Config struct {
	Root             string // repository root
	GpnDir           string // usually Root/.gpn
	PolicyPath       string
	Policy           *policy.Policy
	Catalog          *policy.Catalog
	Store            *store.Store
	Driver           driver.Driver
	Tracer           telemetry.Tracer
	Out              io.Writer
	GpnBinary        string
	ConstitutionPath string
	RouteOverride    string
	ClassOverrides   map[string]string // class -> tier
	DryRun           bool
	Now              func() time.Time
}

// Engine runs workflows.
type Engine struct {
	cfg            Config
	audit          *audit.Chain
	graph          *trace.Graph
	settingsPath   string
	mcpPath        string
	settingsHash   string
	pluginDirs     []string
	runtimeVersion string
}

// New prepares an engine: compiles the policy to Claude Code settings and
// collects installed, verified plugin directories.
func New(cfg Config) (*Engine, error) {
	if cfg.Out == nil {
		cfg.Out = io.Discard
	}
	if cfg.Tracer == nil {
		cfg.Tracer = telemetry.Nop()
	}
	if cfg.GpnDir == "" {
		cfg.GpnDir = filepath.Join(cfg.Root, ".gpn")
	}
	if cfg.GpnBinary == "" {
		if exe, err := os.Executable(); err == nil {
			cfg.GpnBinary = exe
		} else {
			cfg.GpnBinary = "gpn"
		}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	e := &Engine{cfg: cfg, audit: &audit.Chain{S: cfg.Store}, graph: &trace.Graph{S: cfg.Store}}
	if err := e.compilePolicy(); err != nil {
		return nil, err
	}
	if v, ok := cfg.Driver.(interface{ Version() (string, error) }); ok && !cfg.DryRun {
		ver, err := v.Version()
		if err != nil {
			return nil, err
		}
		e.runtimeVersion = ver
		if err := cfg.Policy.CheckRuntimeVersion(ver); err != nil {
			return nil, err
		}
	}
	skills, err := cfg.Store.ListSkills()
	if err != nil {
		return nil, err
	}
	for _, sk := range skills {
		if sk.Path != "" {
			e.pluginDirs = append(e.pluginDirs, sk.Path)
		}
	}
	return e, nil
}

func (e *Engine) compilePolicy() error {
	if err := os.MkdirAll(e.cfg.GpnDir, 0o755); err != nil {
		return err
	}
	opts := policy.CompileOptions{GpnBinary: e.cfg.GpnBinary, PolicyPath: e.cfg.PolicyPath, DBPath: e.cfg.Store.Path}
	settings, err := e.cfg.Policy.CompileSettings(opts)
	if err != nil {
		return err
	}
	e.settingsPath = filepath.Join(e.cfg.GpnDir, "claude-settings.json")
	if err := os.WriteFile(e.settingsPath, settings, 0o644); err != nil {
		return err
	}
	e.settingsHash = hashBytes(settings)
	mcpCfg, err := e.cfg.Policy.CompileMCPConfig(opts, nil)
	if err != nil {
		return err
	}
	e.mcpPath = filepath.Join(e.cfg.GpnDir, "mcp.json")
	return os.WriteFile(e.mcpPath, mcpCfg, 0o644)
}

// SettingsPath returns the compiled Claude Code settings path.
func (e *Engine) SettingsPath() string { return e.settingsPath }

// Run starts a new run of wf and executes until it finishes, waits or fails.
func (e *Engine) Run(ctx context.Context, wf *workflow.Workflow, wfSource []byte, inputs map[string]string) (string, error) {
	for name, in := range wf.Inputs {
		if in.Required && inputs[name] == "" {
			return "", fmt.Errorf("input %q is required (pass --input %s=<value>)", name, name)
		}
	}
	runID := fmt.Sprintf("RUN-%s-%s", e.cfg.Now().UTC().Format("20060102-150405"), hashBytes([]byte(fmt.Sprint(e.cfg.Now().UnixNano())))[:4])
	inputsJSON, _ := json.Marshal(inputs)
	constitutionHash := ""
	if e.cfg.ConstitutionPath != "" {
		if b, err := os.ReadFile(e.cfg.ConstitutionPath); err == nil {
			constitutionHash = hashBytes(b)
		}
	}
	run := store.Run{ID: runID, Workflow: wf.Name, WorkflowHash: hashBytes(wfSource), PolicyHash: e.cfg.Policy.Hash(), ConstitutionHash: constitutionHash, Status: "running", InputsJSON: string(inputsJSON), StartedAt: e.cfg.Now()}
	if err := e.cfg.Store.CreateRun(run); err != nil {
		return "", err
	}
	e.audit.Append(runID, "", "run_started", map[string]any{"workflow": wf.Name, "workflow_hash": run.WorkflowHash, "policy_hash": run.PolicyHash, "inputs": inputs, "driver": e.cfg.Driver.Name(), "runtime_version": e.runtimeVersion})
	fmt.Fprintf(e.cfg.Out, "run %s started (%s)\n", runID, wf.Name)
	return runID, e.execute(ctx, runID, wf, inputs)
}

// Resume continues a run from its journal.
func (e *Engine) Resume(ctx context.Context, runID string, wf *workflow.Workflow) error {
	run, err := e.cfg.Store.GetRun(runID)
	if err != nil {
		return err
	}
	var inputs map[string]string
	_ = json.Unmarshal([]byte(run.InputsJSON), &inputs)
	if run.Status == "done" {
		fmt.Fprintf(e.cfg.Out, "run %s is already done\n", runID)
		return nil
	}
	if err := e.cfg.Store.SetRunStatus(runID, "running"); err != nil {
		return err
	}
	e.audit.Append(runID, "", "run_resumed", map[string]any{"from_status": run.Status})
	fmt.Fprintf(e.cfg.Out, "run %s resumed\n", runID)
	return e.execute(ctx, runID, wf, inputs)
}

func (e *Engine) execute(ctx context.Context, runID string, wf *workflow.Workflow, inputs map[string]string) error {
	span := e.cfg.Tracer.Start("invoke_workflow", map[string]any{"gpn.run_id": runID, "gpn.workflow": wf.Name})
	defer span.End()
	waves, err := wf.Waves()
	if err != nil {
		return err
	}
	vars := map[string]any{"run": map[string]any{"id": runID}, "inputs": toAny(inputs), "steps": map[string]any{}}
	if err := e.loadOutputs(runID, vars); err != nil {
		return err
	}
	for _, wave := range waves {
		for _, stepID := range wave {
			if e.stopRequested(runID) {
				e.cfg.Store.SetRunStatus(runID, "stopped")
				e.audit.Append(runID, stepID, "run_stopped", nil)
				return ErrStopped
			}
			latest, ok, err := e.cfg.Store.LatestStep(runID, stepID)
			if err != nil {
				return err
			}
			if ok && latest.Status == "done" {
				continue
			}
			step := wf.Step(stepID)
			if err := e.runStep(ctx, runID, wf, step, vars, e.cfg.Root); err != nil {
				switch {
				case errors.Is(err, ErrWaitingApproval):
					e.cfg.Store.SetRunStatus(runID, "waiting_approval")
				case errors.Is(err, ErrBlocked):
					e.cfg.Store.SetRunStatus(runID, "blocked")
				case errors.Is(err, ErrBudget):
					e.cfg.Store.SetRunStatus(runID, "blocked")
				case errors.Is(err, ErrStopped):
					e.cfg.Store.SetRunStatus(runID, "stopped")
				default:
					e.cfg.Store.SetRunStatus(runID, "failed")
				}
				e.audit.Append(runID, stepID, "run_halted", map[string]any{"error": err.Error()})
				return err
			}
		}
	}
	e.cfg.Store.SetRunStatus(runID, "done")
	run, _ := e.cfg.Store.GetRun(runID)
	e.audit.Append(runID, "", "run_done", map[string]any{"cost_usd": run.CostUSD, "vendor_cost_usd": run.VendorCostUSD})
	fmt.Fprintf(e.cfg.Out, "run %s done (cost $%.4f, vendor estimate $%.4f)\n", runID, run.CostUSD, run.VendorCostUSD)
	return nil
}

func (e *Engine) loadOutputs(runID string, vars map[string]any) error {
	steps, err := e.cfg.Store.ListSteps(runID)
	if err != nil {
		return err
	}
	outs := vars["steps"].(map[string]any)
	for _, st := range steps {
		if st.Status != "done" || st.OutputJSON == "" {
			continue
		}
		var out any
		if err := json.Unmarshal([]byte(st.OutputJSON), &out); err != nil {
			out = st.OutputJSON
		}
		outs[st.StepID] = map[string]any{"output": out}
	}
	return nil
}

func (e *Engine) runStep(ctx context.Context, runID string, wf *workflow.Workflow, step *workflow.Step, vars map[string]any, cwd string) error {
	switch step.Kind {
	case "agent":
		out, err := e.runAgent(ctx, runID, wf, step, step.ID, vars, cwd)
		if err != nil {
			return err
		}
		setOutput(vars, step.ID, out)
		return nil
	case "approval":
		return e.runApproval(runID, step)
	case "gate":
		out, err := e.runGate(ctx, runID, step, cwd)
		if err != nil {
			return err
		}
		setOutput(vars, step.ID, out)
		return nil
	case "shell":
		out, err := e.runShell(ctx, runID, step, vars, cwd)
		if err != nil {
			return err
		}
		setOutput(vars, step.ID, out)
		return nil
	case "parallel":
		out, err := e.runParallel(ctx, runID, wf, step, vars)
		if err != nil {
			return err
		}
		setOutput(vars, step.ID, out)
		return nil
	}
	return fmt.Errorf("step %s: unknown kind %q", step.ID, step.Kind)
}

func setOutput(vars map[string]any, stepID string, out json.RawMessage) {
	var v any
	if err := json.Unmarshal(out, &v); err != nil {
		v = string(out)
	}
	vars["steps"].(map[string]any)[stepID] = map[string]any{"output": v}
}

// runAgent executes one Claude Code session for a step.
func (e *Engine) runAgent(ctx context.Context, runID string, wf *workflow.Workflow, step *workflow.Step, stepID string, vars map[string]any, cwd string) (json.RawMessage, error) {
	class := render(step.Class, vars)
	if tier, ok := e.cfg.ClassOverrides[class]; ok {
		class = e.classForTier(class, tier)
	}
	res, err := e.cfg.Policy.ResolveModel(class, e.cfg.Catalog, e.cfg.RouteOverride)
	if err != nil {
		return nil, fmt.Errorf("step %s: %w", stepID, err)
	}
	row, err := e.cfg.Store.BeginStep(runID, stepID, "agent")
	if err != nil {
		return nil, err
	}
	row.Tier, row.Route, row.ModelRequested, row.SettingsHash = res.Tier, res.Route, res.Model, e.settingsHash
	if cwd != e.cfg.Root {
		row.Worktree = cwd
	}
	prompt := e.buildPrompt(step, vars)
	row.InputHash = hashBytes([]byte(prompt))
	budget := step.BudgetUSD
	if budget == 0 {
		budget = wf.Defaults.BudgetUSD
	}
	if budget == 0 {
		budget = e.cfg.Policy.Budgets.PerStepUSD
	}
	spec := driver.StepSpec{
		RunID: runID, StepID: stepID, Prompt: prompt, SystemPromptFile: e.cfg.ConstitutionPath,
		Model: res.Model, FallbackModel: res.Fallback, BudgetUSD: budget,
		OutputSchemaPath: e.abs(step.OutputSchema), SettingsPath: e.settingsPath, MCPConfigPath: e.mcpPath,
		PluginDirs: e.pluginDirs, AllowedTools: step.Tools, PermissionMode: e.cfg.Policy.PermissionMode(), Cwd: cwd,
		Env: routeEnv(res.Route),
	}
	runNode, _ := e.graph.Put(trace.AgentRun, runID+"/"+stepID, map[string]any{"run": runID, "step": stepID, "attempt": row.Attempt, "class": class, "tier": res.Tier, "route": res.Route})
	modelNode, _ := e.graph.Put(trace.Model, res.Model, map[string]any{"route": res.Route})
	e.graph.Link(runNode, modelNode, trace.ExecutedBy, map[string]any{"requested": true})
	e.audit.Append(runID, stepID, "step_started", map[string]any{"attempt": row.Attempt, "class": class, "tier": res.Tier, "route": res.Route, "model_requested": res.Model, "prompt_hash": row.InputHash, "settings_hash": e.settingsHash, "constitution": e.cfg.ConstitutionPath, "plugins": e.pluginDirs, "budget_usd": budget})
	fmt.Fprintf(e.cfg.Out, "  %-28s %-6s %-8s %s\n", stepID, res.Tier, res.Route, res.Model)

	if e.cfg.DryRun {
		row.Status, row.OutputJSON = "done", `{"dry_run":true}`
		now := e.cfg.Now()
		row.FinishedAt = &now
		e.cfg.Store.UpdateStep(row)
		return json.RawMessage(row.OutputJSON), nil
	}

	span := e.cfg.Tracer.Start("invoke_agent", map[string]any{"gpn.run_id": runID, "gpn.step_id": stepID, "gen_ai.request.model": res.Model, "gen_ai.provider.name": res.Route, "gpn.tier": res.Tier})
	defer span.End()

	sess, err := e.cfg.Driver.Start(ctx, spec)
	if err != nil {
		return nil, e.failStep(row, err)
	}
	var initPayload json.RawMessage
	for ev := range sess.Events() {
		if e.stopRequested(runID) {
			sess.Interrupt()
		}
		e.recordEvent(row.ID, ev)
		if ev.Type == driver.EvSystem && ev.Text == "init" {
			initPayload = ev.Payload
		}
	}
	result, err := sess.Wait()
	if err != nil {
		return nil, e.failStep(row, err)
	}
	if result.IsError {
		return nil, e.failStep(row, fmt.Errorf("agent reported an error: %s", truncate(result.ResultText, 400)))
	}
	if e.stopRequested(runID) {
		return nil, e.failStep(row, ErrStopped)
	}
	usd := cost.Compute(result.Usage, res.Entry)
	row.Status = "done"
	row.ModelResponded = firstNonEmpty(result.ModelResponded, res.Model)
	row.SessionID = result.SessionID
	row.CostUSD, row.VendorCostUSD = usd, result.VendorCostUSD
	row.SkillVersions = strings.Join(skillVersions(initPayload, result), ",")
	out := result.StructuredOutput
	if len(out) == 0 {
		b, _ := json.Marshal(map[string]any{"result": result.ResultText})
		out = b
	}
	row.OutputJSON = string(out)
	now := e.cfg.Now()
	row.FinishedAt = &now
	if err := e.cfg.Store.UpdateStep(row); err != nil {
		return nil, err
	}
	e.cfg.Store.AddRunCost(runID, usd, result.VendorCostUSD)
	span.Set("gen_ai.response.model", row.ModelResponded)
	span.Set("gen_ai.usage.input_tokens", result.Usage.InputTokens)
	span.Set("gen_ai.usage.output_tokens", result.Usage.OutputTokens)
	span.Set("gpn.cost_usd", usd)
	respModel, _ := e.graph.Put(trace.Model, row.ModelResponded, nil)
	e.graph.Link(runNode, respModel, trace.ExecutedBy, map[string]any{"responded": true})
	for _, sv := range strings.Split(row.SkillVersions, ",") {
		if sv != "" {
			sk, _ := e.graph.Put(trace.Skill, sv, nil)
			e.graph.Link(runNode, sk, trace.ExecutedBy, map[string]any{"skill": true})
		}
	}
	e.audit.Append(runID, stepID, "step_finished", map[string]any{"attempt": row.Attempt, "model_responded": row.ModelResponded, "session_id": row.SessionID, "cost_usd": usd, "vendor_cost_usd": result.VendorCostUSD, "output_hash": hashBytes(out), "skill_versions": row.SkillVersions, "permission_denials": result.PermissionDenials})
	e.lineageFromOutput(runID, stepID, runNode, out)
	if e.cfg.Policy.Budgets.PerRunUSD > 0 {
		run, _ := e.cfg.Store.GetRun(runID)
		if run.CostUSD >= e.cfg.Policy.Budgets.PerRunUSD {
			return out, fmt.Errorf("%w: $%.2f of $%.2f", ErrBudget, run.CostUSD, e.cfg.Policy.Budgets.PerRunUSD)
		}
		if run.CostUSD >= e.cfg.Policy.Budgets.PerRunUSD*e.cfg.Policy.Budgets.AlertAt {
			fmt.Fprintf(e.cfg.Out, "  budget alert: $%.2f of $%.2f used\n", run.CostUSD, e.cfg.Policy.Budgets.PerRunUSD)
		}
	}
	return out, nil
}

// classForTier finds a class name that maps to the requested tier so an
// override like implement=haiku resolves through the same catalog path.
func (e *Engine) classForTier(class, tier string) string {
	if t, ok := e.cfg.Policy.Models.Classes[class]; ok && t == tier {
		return class
	}
	names := make([]string, 0, len(e.cfg.Policy.Models.Classes))
	for c, t := range e.cfg.Policy.Models.Classes {
		if t == tier {
			names = append(names, c)
		}
	}
	sort.Strings(names)
	if len(names) > 0 {
		return names[0]
	}
	return class
}

func (e *Engine) failStep(row store.Step, err error) error {
	row.Status = "failed"
	row.Error = err.Error()
	now := e.cfg.Now()
	row.FinishedAt = &now
	e.cfg.Store.UpdateStep(row)
	e.audit.Append(row.RunID, row.StepID, "step_failed", map[string]any{"attempt": row.Attempt, "error": err.Error()})
	return fmt.Errorf("step %s: %w", row.StepID, err)
}

func (e *Engine) recordEvent(stepRowID int64, ev driver.Event) {
	payload := ev.Payload
	if len(payload) == 0 {
		b, _ := json.Marshal(ev)
		payload = b
	}
	e.cfg.Store.InsertEvent(store.Event{StepRowID: stepRowID, Seq: ev.Seq, Type: string(ev.Type), ToolName: ev.ToolName, ToolUseID: ev.ToolUseID, InputHash: hashBytes(ev.Input), OutputHash: hashBytes(ev.Output), Payload: truncate(string(payload), 64*1024), Time: ev.Time})
}

// buildPrompt renders the step's prompt file (if any) and appends the
// inputs and upstream outputs the step depends on as JSON context.
func (e *Engine) buildPrompt(step *workflow.Step, vars map[string]any) string {
	var b strings.Builder
	if step.Skill != "" {
		b.WriteString("/" + skillCommand(step.Skill) + "\n\n")
	}
	if step.Prompt != "" {
		if data, err := os.ReadFile(e.abs(step.Prompt)); err == nil {
			b.WriteString(render(string(data), vars))
			b.WriteString("\n\n")
		} else {
			b.WriteString(render(step.Prompt, vars))
			b.WriteString("\n\n")
		}
	}
	b.WriteString("## Run context\n")
	b.WriteString("Run: " + render("${run.id}", vars) + "\n")
	if inputs, ok := vars["inputs"].(map[string]any); ok && len(inputs) > 0 {
		j, _ := json.MarshalIndent(inputs, "", "  ")
		b.WriteString("Inputs:\n```json\n" + string(j) + "\n```\n")
	}
	if item, ok := vars["item"]; ok {
		j, _ := json.MarshalIndent(item, "", "  ")
		b.WriteString("Task:\n```json\n" + string(j) + "\n```\n")
	}
	for _, dep := range step.After {
		if so, ok := vars["steps"].(map[string]any)[dep]; ok {
			j, _ := json.MarshalIndent(so, "", "  ")
			b.WriteString(fmt.Sprintf("Output of step %s:\n```json\n%s\n```\n", dep, truncate(string(j), 20000)))
		}
	}
	if step.OutputSchema != "" {
		b.WriteString("Respond with JSON that matches the required output schema.\n")
	}
	return b.String()
}

func skillCommand(ref string) string {
	// gpn/sdd-intake@0.1.0 -> sdd-intake
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		ref = ref[i+1:]
	}
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	return ref
}

func (e *Engine) runApproval(runID string, step *workflow.Step) error {
	a, ok, err := e.cfg.Store.LatestApproval(runID, step.ID)
	if err != nil {
		return err
	}
	if !ok {
		req, _ := json.Marshal(map[string]any{"approvers": step.Approvers, "timeout": step.Timeout})
		a, err = e.cfg.Store.RequestApproval(store.Approval{RunID: runID, StepID: step.ID, Class: "workflow", ToolCallJSON: string(req)})
		if err != nil {
			return err
		}
		e.audit.Append(runID, step.ID, "approval_requested", map[string]any{"approval_id": a.ID, "approvers": step.Approvers})
	}
	switch a.Decision {
	case "":
		fmt.Fprintf(e.cfg.Out, "  %-28s waiting for approval #%d (%s) — gpn approve %s %s --by <you>\n", step.ID, a.ID, strings.Join(step.Approvers, ", "), runID, step.ID)
		return ErrWaitingApproval
	case "deny":
		row, _ := e.cfg.Store.BeginStep(runID, step.ID, "approval")
		return e.failStep(row, fmt.Errorf("denied by %s: %s", a.Decider, a.Reason))
	}
	row, err := e.cfg.Store.BeginStep(runID, step.ID, "approval")
	if err != nil {
		return err
	}
	row.Status = "done"
	out, _ := json.Marshal(map[string]any{"approval_id": a.ID, "decider": a.Decider, "reason": a.Reason})
	row.OutputJSON = string(out)
	now := e.cfg.Now()
	row.FinishedAt = &now
	if err := e.cfg.Store.UpdateStep(row); err != nil {
		return err
	}
	apNode, _ := e.graph.Put(trace.Approval, fmt.Sprintf("%d", a.ID), map[string]any{"decider": a.Decider, "class": a.Class})
	runNode, _ := e.graph.Put(trace.AgentRun, runID+"/"+step.ID, map[string]any{"run": runID, "step": step.ID, "kind": "approval"})
	e.graph.Link(runNode, apNode, trace.ApprovedBy, nil)
	e.audit.Append(runID, step.ID, "approval_decided", map[string]any{"approval_id": a.ID, "decider": a.Decider, "decision": a.Decision, "reason": a.Reason})
	fmt.Fprintf(e.cfg.Out, "  %-28s approved by %s\n", step.ID, a.Decider)
	return nil
}

func (e *Engine) runGate(ctx context.Context, runID string, step *workflow.Step, cwd string) (json.RawMessage, error) {
	row, err := e.cfg.Store.BeginStep(runID, step.ID, "gate")
	if err != nil {
		return nil, err
	}
	var results []gates.Result
	failed := false
	for _, check := range step.Checks {
		name, cmd, err := e.cfg.Policy.GateCommand(check)
		if err != nil {
			return nil, e.failStep(row, err)
		}
		r := gates.Run(ctx, check, name, cmd, cwd)
		results = append(results, r)
		fmt.Fprintf(e.cfg.Out, "  gate %s\n", r.String())
		trNode, _ := e.graph.Put(trace.TestRun, runID+"/"+step.ID+"/"+check, map[string]any{"status": r.Status, "value": r.Value, "command": cmd})
		runNode, _ := e.graph.Put(trace.AgentRun, runID+"/"+step.ID, map[string]any{"run": runID, "step": step.ID, "kind": "gate"})
		e.graph.Link(runNode, trNode, trace.ExecutedBy, nil)
		if r.Status == "pass" {
			e.linkRequirementsVerified(runID, trNode)
		} else {
			failed = true
		}
	}
	out, _ := json.Marshal(map[string]any{"results": results, "passed": !failed})
	e.audit.Append(runID, step.ID, "gate_results", json.RawMessage(out))
	if failed {
		row.Status = "blocked"
		row.OutputJSON = string(out)
		row.Error = "gate failed"
		now := e.cfg.Now()
		row.FinishedAt = &now
		e.cfg.Store.UpdateStep(row)
		return out, fmt.Errorf("step %s: %w", step.ID, ErrBlocked)
	}
	row.Status = "done"
	row.OutputJSON = string(out)
	now := e.cfg.Now()
	row.FinishedAt = &now
	return out, e.cfg.Store.UpdateStep(row)
}

func (e *Engine) runShell(ctx context.Context, runID string, step *workflow.Step, vars map[string]any, cwd string) (json.RawMessage, error) {
	row, err := e.cfg.Store.BeginStep(runID, step.ID, "shell")
	if err != nil {
		return nil, err
	}
	command := render(step.Command, vars)
	var out []byte
	if strings.HasPrefix(command, "gpn internal merge") {
		merged, merr := e.mergeWorktrees(runID)
		out, _ = json.Marshal(map[string]any{"merged": merged})
		err = merr
	} else {
		cmd := exec.CommandContext(ctx, "sh", "-c", command)
		cmd.Dir = cwd
		cmd.Env = append(os.Environ(), "GPN_RUN_ID="+runID, "GPN_STEP_ID="+step.ID)
		var combined []byte
		combined, err = cmd.CombinedOutput()
		out, _ = json.Marshal(map[string]any{"command": command, "output": truncate(string(combined), 20000), "ok": err == nil})
	}
	e.audit.Append(runID, step.ID, "shell", map[string]any{"command": command, "ok": err == nil})
	if err != nil {
		return nil, e.failStep(row, fmt.Errorf("%s: %w", command, err))
	}
	row.Status = "done"
	row.OutputJSON = string(out)
	now := e.cfg.Now()
	row.FinishedAt = &now
	fmt.Fprintf(e.cfg.Out, "  %-28s ok\n", step.ID)
	return out, e.cfg.Store.UpdateStep(row)
}

func (e *Engine) runParallel(ctx context.Context, runID string, wf *workflow.Workflow, step *workflow.Step, vars map[string]any) (json.RawMessage, error) {
	itemsAny := eval(step.ForEach, vars)
	items, ok := itemsAny.([]any)
	if !ok {
		return nil, fmt.Errorf("step %s: for_each %q did not resolve to a list", step.ID, step.ForEach)
	}
	conc := step.MaxConcurrency
	if conc <= 0 {
		conc = 4
	}
	type result struct {
		key string
		out json.RawMessage
		err error
	}
	results := make([]result, len(items))
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	base := ""
	if step.Isolation == "worktree" {
		h, err := vcs.Repo{Dir: e.cfg.Root}.Head()
		if err != nil {
			return nil, fmt.Errorf("step %s: worktree isolation needs a git repository: %w", step.ID, err)
		}
		base = h
	}
	for i, item := range items {
		key := itemKey(item, i)
		childID := step.ID + "[" + key + "]"
		if latest, ok, _ := e.cfg.Store.LatestStep(runID, childID); ok && latest.Status == "done" {
			results[i] = result{key: key, out: json.RawMessage(latest.OutputJSON)}
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, item any, key, childID string) {
			defer wg.Done()
			defer func() { <-sem }()
			childVars := cloneVars(vars)
			childVars["item"] = item
			cwd := e.cfg.Root
			if step.Isolation == "worktree" {
				cwd = filepath.Join(e.cfg.GpnDir, "wt", runID, key)
				if _, err := os.Stat(cwd); err != nil {
					if err := (vcs.Repo{Dir: e.cfg.Root}).AddWorktree(cwd, "gpn/"+runID+"/"+key, base); err != nil {
						results[i] = result{key: key, err: fmt.Errorf("%s: worktree: %w", childID, err)}
						return
					}
				}
			}
			child := *step.Step
			child.ID = childID
			child.After = step.After
			var out json.RawMessage
			var err error
			switch child.Kind {
			case "agent":
				out, err = e.runAgent(ctx, runID, wf, &child, childID, childVars, cwd)
			case "shell":
				out, err = e.runShell(ctx, runID, &child, childVars, cwd)
			case "gate":
				out, err = e.runGate(ctx, runID, &child, cwd)
			default:
				err = fmt.Errorf("%s: unsupported child kind %q", childID, child.Kind)
			}
			if err == nil && step.Isolation == "worktree" {
				err = e.commitWorktree(runID, childID, cwd, item)
			}
			results[i] = result{key: key, out: out, err: err}
		}(i, item, key, childID)
	}
	wg.Wait()
	outs := make([]any, 0, len(results))
	for _, r := range results {
		if r.err != nil {
			return nil, r.err
		}
		var v any
		if err := json.Unmarshal(r.out, &v); err != nil {
			v = string(r.out)
		}
		outs = append(outs, map[string]any{"key": r.key, "output": v})
	}
	row, err := e.cfg.Store.BeginStep(runID, step.ID, "parallel")
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(outs)
	row.Status = "done"
	row.OutputJSON = string(b)
	now := e.cfg.Now()
	row.FinishedAt = &now
	return b, e.cfg.Store.UpdateStep(row)
}

func (e *Engine) commitWorktree(runID, stepID, cwd string, item any) error {
	trailers := map[string]string{"GPN-Run": runID, "GPN-Step": stepID}
	title := stepID
	if m, ok := item.(map[string]any); ok {
		if id, ok := m["id"].(string); ok {
			trailers["GPN-Task"] = id
		}
		if t, ok := m["title"].(string); ok {
			title = t
		}
		if reqs, ok := m["requirements"].([]any); ok {
			var rs []string
			for _, r := range reqs {
				if s, ok := r.(string); ok {
					rs = append(rs, s)
				}
			}
			if len(rs) > 0 {
				trailers["GPN-Req"] = strings.Join(rs, ",")
			}
		}
	}
	hash, err := (vcs.Repo{Dir: cwd}).Commit(title, trailers)
	if err != nil {
		return fmt.Errorf("%s: commit: %w", stepID, err)
	}
	if hash == "" {
		return nil
	}
	commitNode, _ := e.graph.Put(trace.Commit, hash, map[string]any{"run": runID, "step": stepID, "title": title})
	runNode := trace.ID(trace.AgentRun, runID+"/"+stepID)
	e.graph.Link(commitNode, runNode, trace.ExecutedBy, nil)
	if t := trailers["GPN-Task"]; t != "" {
		e.graph.Link(trace.ID(trace.Task, t), commitNode, trace.ChangedIn, nil)
	}
	e.audit.Append(runID, stepID, "commit", map[string]any{"hash": hash, "trailers": trailers})
	return nil
}

// mergeWorktrees merges every task branch of the run into the current branch
// and removes the worktrees.
func (e *Engine) mergeWorktrees(runID string) ([]string, error) {
	root := vcs.Repo{Dir: e.cfg.Root}
	wtRoot := filepath.Join(e.cfg.GpnDir, "wt", runID)
	entries, err := os.ReadDir(wtRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var merged []string
	for _, en := range entries {
		if !en.IsDir() {
			continue
		}
		branch := "gpn/" + runID + "/" + en.Name()
		before, _ := root.Head()
		if err := root.Merge(branch, "Merge "+branch+"\n\nGPN-Run: "+runID); err != nil {
			return merged, fmt.Errorf("merge %s: %w", branch, err)
		}
		after, _ := root.Head()
		if files, err := root.ChangedFiles(before, after); err == nil {
			for _, f := range files {
				fid, _ := e.graph.Put(trace.File, f, nil)
				e.graph.Link(fid, trace.ID(trace.Commit, after), trace.ChangedIn, nil)
			}
		}
		root.RemoveWorktree(filepath.Join(wtRoot, en.Name()))
		merged = append(merged, branch)
	}
	return merged, nil
}

// lineageFromOutput reads known structured-output shapes and writes graph
// nodes and edges: requirements, tasks (with files and requirement links),
// and design components.
func (e *Engine) lineageFromOutput(runID, stepID, runNode string, out json.RawMessage) {
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		return
	}
	if reqs, ok := doc["requirements"].([]any); ok {
		for _, r := range reqs {
			m, _ := r.(map[string]any)
			id, _ := m["id"].(string)
			if id == "" {
				continue
			}
			n, _ := e.graph.Put(trace.Requirement, id, map[string]any{"text": m["text"], "priority": m["priority"], "run": runID})
			e.graph.Link(n, runNode, trace.DerivedFrom, nil)
		}
	}
	if tasks, ok := doc["tasks"].([]any); ok {
		for _, t := range tasks {
			m, _ := t.(map[string]any)
			id, _ := m["id"].(string)
			if id == "" {
				continue
			}
			tn, _ := e.graph.Put(trace.Task, id, map[string]any{"title": m["title"], "class": m["class"], "run": runID})
			e.graph.Link(tn, runNode, trace.DerivedFrom, nil)
			if reqs, ok := m["requirements"].([]any); ok {
				for _, r := range reqs {
					if rid, ok := r.(string); ok {
						e.graph.Link(trace.ID(trace.Requirement, rid), tn, trace.SatisfiedBy, nil)
					}
				}
			}
			if files, ok := m["files"].([]any); ok {
				for _, f := range files {
					if path, ok := f.(string); ok {
						fn, _ := e.graph.Put(trace.File, path, nil)
						e.graph.Link(tn, fn, trace.ImplementedIn, nil)
					}
				}
			}
			if deps, ok := m["depends_on"].([]any); ok {
				for _, d := range deps {
					if did, ok := d.(string); ok {
						e.graph.Link(tn, trace.ID(trace.Task, did), trace.DependsOn, nil)
					}
				}
			}
		}
	}
	if comps, ok := doc["components"].([]any); ok {
		for _, c := range comps {
			m, _ := c.(map[string]any)
			name, _ := m["name"].(string)
			if name == "" {
				continue
			}
			dn, _ := e.graph.Put(trace.Decision, name, map[string]any{"run": runID, "step": stepID})
			if reqs, ok := m["requirements"].([]any); ok {
				for _, r := range reqs {
					if rid, ok := r.(string); ok {
						e.graph.Link(trace.ID(trace.Requirement, rid), dn, trace.Refines, nil)
					}
				}
			}
		}
	}
}

// linkRequirementsVerified links every requirement produced by the run to a
// passing TestRun. The prototype verifies at run granularity; test-name
// matching per requirement ID is the week-4 refinement.
func (e *Engine) linkRequirementsVerified(runID, testRunNode string) {
	// Collect first, then write: the store runs on one connection, so a
	// write while the cursor is open would wait forever.
	rows, err := e.cfg.Store.DB.Query(`SELECT id FROM nodes WHERE type='Requirement' AND json_extract(props_json,'$.run')=?`, runID)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		e.graph.Link(id, testRunNode, trace.VerifiedBy, nil)
	}
}

// RequestStop asks a running engine (in this or another process) to stop the
// run at the next safe point.
func RequestStop(gpnDir, runID string) error {
	dir := filepath.Join(gpnDir, "stop")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, runID), []byte(time.Now().UTC().Format(time.RFC3339)), 0o644)
}

// ClearStop removes a stop request.
func ClearStop(gpnDir, runID string) { os.Remove(filepath.Join(gpnDir, "stop", runID)) }

func (e *Engine) stopRequested(runID string) bool {
	_, err := os.Stat(filepath.Join(e.cfg.GpnDir, "stop", runID))
	return err == nil
}

func (e *Engine) abs(p string) string {
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(e.cfg.Root, p)
}

func routeEnv(route string) map[string]string {
	switch route {
	case "bedrock":
		return map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}
	case "vertex":
		return map[string]string{"CLAUDE_CODE_USE_VERTEX": "1"}
	default:
		return map[string]string{"CLAUDE_CODE_USE_BEDROCK": "", "CLAUDE_CODE_USE_VERTEX": ""}
	}
}

func skillVersions(initPayload json.RawMessage, res driver.Result) []string {
	if len(res.SkillVersions) > 0 {
		return res.SkillVersions
	}
	if len(initPayload) == 0 {
		return nil
	}
	var m struct {
		Plugins []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"plugins"`
	}
	if json.Unmarshal(initPayload, &m) != nil {
		return nil
	}
	var out []string
	for _, p := range m.Plugins {
		if p.Version != "" {
			out = append(out, p.Name+"@"+p.Version)
		} else {
			out = append(out, p.Name)
		}
	}
	return out
}

// --- templating ---------------------------------------------------------

var tmpl = regexp.MustCompile(`\$\{([^}]+)\}`)

// render substitutes ${a.b.c} references with string values from vars.
func render(s string, vars map[string]any) string {
	return tmpl.ReplaceAllStringFunc(s, func(m string) string {
		v := eval(m, vars)
		switch t := v.(type) {
		case nil:
			return ""
		case string:
			return t
		default:
			b, _ := json.Marshal(t)
			return string(b)
		}
	})
}

// eval resolves a ${path | default} expression against vars.
func eval(expr string, vars map[string]any) any {
	expr = strings.TrimSpace(expr)
	if !strings.HasPrefix(expr, "${") || !strings.HasSuffix(expr, "}") {
		return expr
	}
	inner := strings.TrimSpace(expr[2 : len(expr)-1])
	def := ""
	if i := strings.Index(inner, "|"); i >= 0 {
		def = strings.TrimSpace(inner[i+1:])
		inner = strings.TrimSpace(inner[:i])
	}
	var cur any = vars
	for _, part := range strings.Split(inner, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			cur = nil
			break
		}
		cur, ok = m[part]
		if !ok {
			cur = nil
			break
		}
	}
	if cur == nil && def != "" {
		return def
	}
	return cur
}

func itemKey(item any, i int) string {
	if m, ok := item.(map[string]any); ok {
		for _, k := range []string{"id", "key", "name"} {
			if s, ok := m[k].(string); ok && s != "" {
				return sanitize(s)
			}
		}
	}
	if s, ok := item.(string); ok {
		return sanitize(s)
	}
	return fmt.Sprintf("%d", i)
}

var unsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func sanitize(s string) string { return unsafe.ReplaceAllString(s, "-") }

func cloneVars(v map[string]any) map[string]any {
	out := make(map[string]any, len(v)+1)
	for k, val := range v {
		out[k] = val
	}
	return out
}

func toAny(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func hashBytes(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

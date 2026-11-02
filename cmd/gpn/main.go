// Command gpn is the GPN Agentic Harness prototype: one binary that drives
// Claude Code headless through governed, journaled, traceable workflows.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/gpn/gpn/internal/assets"
	"github.com/gpn/gpn/internal/audit"
	"github.com/gpn/gpn/internal/driver"
	"github.com/gpn/gpn/internal/driver/claudecode"
	"github.com/gpn/gpn/internal/driver/mock"
	"github.com/gpn/gpn/internal/engine"
	"github.com/gpn/gpn/internal/evals"
	"github.com/gpn/gpn/internal/hook"
	"github.com/gpn/gpn/internal/mcp"
	"github.com/gpn/gpn/internal/policy"
	"github.com/gpn/gpn/internal/registry"
	"github.com/gpn/gpn/internal/store"
	"github.com/gpn/gpn/internal/telemetry"
	"github.com/gpn/gpn/internal/trace"
	"github.com/gpn/gpn/internal/vcs"
	"github.com/gpn/gpn/internal/workflow"
)

var version = "0.1.1-dev"

const usage = `gpn — GPN Agentic Harness (prototype)

Usage:
  gpn init [--force]                         set up this repository (policy, workflows, schemas, seed registry, keys)
  gpn run <workflow> [--input k=v]... [--route R] [--set classes.<class>=<tier>] [--driver claude|mock] [--dry-run]
  gpn resume <run-id>                        continue a run from its journal
  gpn status [run-id]                        list runs, or show one run's steps and approvals
  gpn approve <run-id> <step-id> --by <who> [--reason text] [--deny] [--id N]
  gpn stop <run-id>                          stop a run at the next safe point
  gpn trace why <file>:<line> | coverage | orphans | impact <node-id> | blast <file>
  gpn audit verify
  gpn registry keygen [id] | publish <dir> [--key id] [--signer name] | install <dir> | verify | list
  gpn eval <plugin-dir> [--judge-model m] [--runs n] [--threshold t] [--max-cost usd] [--dry-run]
  gpn policy compile | check <tool> '<json tool_input>'
  gpn hook pre|permission|post|stop --policy P --db D   (called by Claude Code)
  gpn mcp --policy P --db D                              (called by Claude Code)
  gpn version

Common flags: --root <dir> (default: current directory), --policy <file>, --db <file>.
`

type common struct {
	root, policyPath, dbPath string
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var err error
	switch cmd {
	case "version", "--version", "-v":
		fmt.Println("gpn", version)
		d := claudecode.New()
		if v, err := d.Version(); err == nil {
			fmt.Printf("claude %s (bare mode: %v, fallback-model: %v, json-schema: %v)\n", v, d.Supports("--bare"), d.Supports("--fallback-model"), d.Supports("--json-schema"))
		} else {
			fmt.Println("claude: not found on PATH")
		}
	case "init":
		err = cmdInit(args)
	case "run":
		err = cmdRun(ctx, args)
	case "resume":
		err = cmdResume(ctx, args)
	case "status":
		err = cmdStatus(args)
	case "approve":
		err = cmdApprove(args)
	case "stop":
		err = cmdStop(args)
	case "trace":
		err = cmdTrace(args)
	case "audit":
		err = cmdAudit(args)
	case "registry":
		err = cmdRegistry(args)
	case "eval":
		err = cmdEval(ctx, args)
	case "policy":
		err = cmdPolicy(args)
	case "hook":
		os.Exit(cmdHook(ctx, args))
	case "mcp":
		err = cmdMCP(args)
	case "internal":
		err = cmdInternal(args)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		switch {
		case errors.Is(err, engine.ErrWaitingApproval):
			fmt.Fprintln(os.Stderr, "waiting:", err)
			os.Exit(3)
		case errors.Is(err, engine.ErrBlocked), errors.Is(err, engine.ErrBudget):
			fmt.Fprintln(os.Stderr, "blocked:", err)
			os.Exit(4)
		case errors.Is(err, engine.ErrStopped):
			fmt.Fprintln(os.Stderr, "stopped:", err)
			os.Exit(5)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// parse accepts flags anywhere on the line (Go's flag package stops at the
// first positional argument), so `gpn run feature --input k=v` works.
func parse(fs *flag.FlagSet, args []string) error {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			continue
		}
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return fs.Parse(append(flags, pos...))
}

func bindCommon(fs *flag.FlagSet) *common {
	c := &common{}
	fs.StringVar(&c.root, "root", "", "repository root (default: current directory)")
	fs.StringVar(&c.policyPath, "policy", "", "policy file (default: <root>/gpn-policy.yaml)")
	fs.StringVar(&c.dbPath, "db", "", "database file (default: <root>/.gpn/gpn.db)")
	return c
}

func (c *common) resolve() error {
	if c.root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		c.root = wd
	}
	abs, err := filepath.Abs(c.root)
	if err != nil {
		return err
	}
	c.root = abs
	if c.policyPath == "" {
		c.policyPath = filepath.Join(c.root, "gpn-policy.yaml")
	}
	if c.dbPath == "" {
		c.dbPath = filepath.Join(c.root, ".gpn", "gpn.db")
	}
	return nil
}

func (c *common) open() (*policy.Policy, *store.Store, error) {
	p, err := policy.Load(c.policyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load policy: %w (run `gpn init` first?)", err)
	}
	st, err := store.Open(c.dbPath)
	if err != nil {
		return nil, nil, err
	}
	return p, st, nil
}

func keysDir() string {
	if d := os.Getenv("GPN_KEYS_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gpn", "keys")
}

// ---- init ---------------------------------------------------------------

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	c := bindCommon(fs)
	force := fs.Bool("force", false, "overwrite seed files that already exist")
	parse(fs, args)
	if err := c.resolve(); err != nil {
		return err
	}
	repo := vcs.Repo{Dir: c.root}
	if !repo.IsRepo() {
		if err := vcs.Init(c.root); err != nil {
			return fmt.Errorf("git init: %w", err)
		}
		fmt.Println("initialized git repository")
	}
	for _, pair := range [][2]string{
		{"gpn-policy.yaml", "gpn-policy.yaml"},
		{"gpn-constitution.md", "gpn-constitution.md"},
		{"model-catalog.json", ".gpn/model-catalog.json"},
		{"workflows", "workflows"},
		{"schemas", "schemas"},
		{"prompts", "prompts"},
		{"registry/sdd-core", ".gpn/registry/sdd-core"},
	} {
		if err := assets.Extract(pair[0], filepath.Join(c.root, pair[1]), *force); err != nil {
			return fmt.Errorf("write %s: %w", pair[1], err)
		}
	}
	gitignore := filepath.Join(c.root, ".gpn", ".gitignore")
	if _, err := os.Stat(gitignore); err != nil {
		os.WriteFile(gitignore, []byte("gpn.db*\nwt/\nstop/\ntraces/\ncover.out\nclaude-settings.json\nmcp.json\n"), 0o644)
	}
	if _, err := registry.LoadKeys(keysDir(), "local"); err != nil {
		if _, err := registry.GenerateKeys(keysDir(), "local"); err != nil {
			return fmt.Errorf("generate keys: %w", err)
		}
		fmt.Println("generated signing key pair", filepath.Join(keysDir(), "local.{key,pub}"))
	}
	p, st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	exe, _ := os.Executable()
	eng, err := engine.New(engine.Config{Root: c.root, PolicyPath: c.policyPath, Policy: p, Store: st, GpnBinary: exe})
	if err != nil {
		return err
	}
	fmt.Println("compiled policy to", eng.SettingsPath())
	// Publish and install the seed plugin so the first run has verified skills.
	keys, _ := registry.LoadKeys(keysDir(), "local")
	pluginDir := filepath.Join(c.root, ".gpn", "registry", "sdd-core")
	if _, _, err := registry.Publish(pluginDir, keys, os.Getenv("USER")); err != nil {
		return fmt.Errorf("publish seed plugin: %w", err)
	}
	if _, err := registry.Install(st, pluginDir, keys, p.Registry.TiersAllowed, p.Registry.RequireSignature); err != nil {
		return fmt.Errorf("install seed plugin: %w", err)
	}
	fmt.Println("installed sdd-core@0.1.0 (signed, tier official)")
	fmt.Println("next: edit gpn-policy.yaml and .gpn/model-catalog.json, then `gpn run feature --input jira=<KEY>`")
	return nil
}

// ---- run / resume -------------------------------------------------------

type kv map[string]string

func (k kv) String() string { return fmt.Sprint(map[string]string(k)) }
func (k kv) Set(s string) error {
	i := strings.IndexAny(s, "=:")
	if i <= 0 {
		return fmt.Errorf("expected key=value, got %q", s)
	}
	k[s[:i]] = s[i+1:]
	return nil
}

func buildEngine(c *common, p *policy.Policy, st *store.Store, driverName, route string, sets kv, dryRun bool) (*engine.Engine, error) {
	cat, err := p.LoadCatalog(filepath.Dir(c.policyPath))
	if err != nil {
		return nil, err
	}
	var drv driver.Driver
	switch driverName {
	case "", "claude", "claude-code":
		cc := claudecode.New()
		cc.Logf = func(format string, args ...any) { fmt.Fprintf(os.Stderr, "warning: "+format+"\n", args...) }
		drv = cc
	case "mock":
		drv = mock.New()
	default:
		return nil, fmt.Errorf("unknown driver %q", driverName)
	}
	overrides := map[string]string{}
	for k, v := range sets {
		if strings.HasPrefix(k, "classes.") {
			overrides[strings.TrimPrefix(k, "classes.")] = v
		}
	}
	tracesDir := filepath.Join(c.root, ".gpn", "traces")
	os.MkdirAll(tracesDir, 0o755)
	tr, err := telemetry.NewFile(filepath.Join(tracesDir, time.Now().UTC().Format("20060102")+".jsonl"))
	if err != nil {
		return nil, err
	}
	exe, _ := os.Executable()
	constitution := filepath.Join(c.root, "gpn-constitution.md")
	if _, err := os.Stat(constitution); err != nil {
		constitution = ""
	}
	return engine.New(engine.Config{Root: c.root, PolicyPath: c.policyPath, Policy: p, Catalog: cat, Store: st, Driver: drv, Tracer: tr, Out: os.Stdout, GpnBinary: exe, ConstitutionPath: constitution, RouteOverride: route, ClassOverrides: overrides, DryRun: dryRun})
}

func loadWorkflow(root, name string) (*workflow.Workflow, []byte, error) {
	candidates := []string{name, filepath.Join(root, "workflows", name+".yaml"), filepath.Join(root, "workflows", name)}
	for _, cnd := range candidates {
		if b, err := os.ReadFile(cnd); err == nil {
			wf, err := workflow.Parse(b)
			return wf, b, err
		}
	}
	return nil, nil, fmt.Errorf("workflow %q not found (looked in workflows/)", name)
}

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	c := bindCommon(fs)
	inputs := kv{}
	sets := kv{}
	fs.Var(inputs, "input", "workflow input key=value (repeatable)")
	fs.Var(sets, "set", "override, e.g. classes.implement=haiku (repeatable)")
	route := fs.String("route", "", "model route override (bedrock | anthropic)")
	drv := fs.String("driver", "claude", "driver: claude or mock")
	dry := fs.Bool("dry-run", false, "resolve models and journal steps without calling Claude Code")
	parse(fs, args)
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: gpn run <workflow> [--input k=v]")
	}
	if err := c.resolve(); err != nil {
		return err
	}
	p, st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	wf, src, err := loadWorkflow(c.root, fs.Arg(0))
	if err != nil {
		return err
	}
	eng, err := buildEngine(c, p, st, *drv, *route, sets, *dry)
	if err != nil {
		return err
	}
	_, err = eng.Run(ctx, wf, src, inputs)
	return err
}

func cmdResume(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("resume", flag.ExitOnError)
	c := bindCommon(fs)
	route := fs.String("route", "", "model route override")
	drv := fs.String("driver", "claude", "driver: claude or mock")
	parse(fs, args)
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: gpn resume <run-id>")
	}
	if err := c.resolve(); err != nil {
		return err
	}
	p, st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	run, err := st.GetRun(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("run %s not found (gpn status lists runs)", fs.Arg(0))
	}
	wf, _, err := loadWorkflow(c.root, run.Workflow)
	if err != nil {
		return err
	}
	engine.ClearStop(filepath.Join(c.root, ".gpn"), run.ID)
	eng, err := buildEngine(c, p, st, *drv, *route, kv{}, false)
	if err != nil {
		return err
	}
	return eng.Resume(ctx, run.ID, wf)
}

// ---- status / approve / stop --------------------------------------------

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	c := bindCommon(fs)
	asJSON := fs.Bool("json", false, "print JSON")
	parse(fs, args)
	if err := c.resolve(); err != nil {
		return err
	}
	_, st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	defer w.Flush()
	if fs.NArg() == 0 {
		runs, err := st.ListRuns(20)
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(runs)
		}
		fmt.Fprintln(w, "RUN\tWORKFLOW\tSTATUS\tSTARTED\tCOST")
		for _, r := range runs {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t$%.4f\n", r.ID, r.Workflow, r.Status, r.StartedAt.Local().Format("Jan 2 15:04"), r.CostUSD)
		}
		return nil
	}
	run, err := st.GetRun(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("run %s not found (gpn status lists runs)", fs.Arg(0))
	}
	steps, err := st.ListSteps(run.ID)
	if err != nil {
		return err
	}
	pend, _ := st.ListPendingApprovals(run.ID)
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"run": run, "steps": steps, "pending_approvals": pend})
	}
	fmt.Fprintf(w, "run %s\t%s\tstatus %s\tcost $%.4f (vendor $%.4f)\n\n", run.ID, run.Workflow, run.Status, run.CostUSD, run.VendorCostUSD)
	fmt.Fprintln(w, "STEP\tKIND\tSTATUS\tTIER\tMODEL\tCOST\tERROR")
	for _, s := range steps {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t$%.4f\t%s\n", s.StepID, s.Kind, s.Status, s.Tier, s.ModelResponded, s.CostUSD, s.Error)
	}
	if len(pend) > 0 {
		fmt.Fprintln(w, "\nPENDING APPROVALS")
		for _, a := range pend {
			fmt.Fprintf(w, "#%d\t%s\t%s\t%s\n", a.ID, a.StepID, a.Class, a.RequestedAt.Local().Format(time.RFC822))
		}
	}
	return nil
}

func cmdApprove(args []string) error {
	fs := flag.NewFlagSet("approve", flag.ExitOnError)
	c := bindCommon(fs)
	by := fs.String("by", "", "decider (required)")
	reason := fs.String("reason", "", "reason")
	deny := fs.Bool("deny", false, "deny instead of approve")
	id := fs.Int64("id", 0, "approval id (default: the pending approval for the step)")
	parse(fs, args)
	if fs.NArg() < 2 || *by == "" {
		return fmt.Errorf("usage: gpn approve <run-id> <step-id> --by <who> [--reason text] [--deny]")
	}
	if err := c.resolve(); err != nil {
		return err
	}
	_, st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	runID, stepID := fs.Arg(0), fs.Arg(1)
	if *id == 0 {
		pend, err := st.ListPendingApprovals(runID)
		if err != nil {
			return err
		}
		for _, a := range pend {
			if a.StepID == stepID {
				*id = a.ID
				break
			}
		}
		if *id == 0 {
			return fmt.Errorf("no pending approval for %s %s", runID, stepID)
		}
	}
	decision := "approve"
	if *deny {
		decision = "deny"
	}
	if err := st.DecideApproval(*id, *by, decision, *reason); err != nil {
		return err
	}
	ch := &audit.Chain{S: st}
	ch.Append(runID, stepID, "approval_decided", map[string]any{"approval_id": *id, "decider": *by, "decision": decision, "reason": *reason})
	fmt.Printf("approval #%d: %s by %s\n", *id, decision, *by)
	if decision == "approve" {
		fmt.Printf("resume with: gpn resume %s\n", runID)
	}
	return nil
}

func cmdStop(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	c := bindCommon(fs)
	parse(fs, args)
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: gpn stop <run-id>")
	}
	if err := c.resolve(); err != nil {
		return err
	}
	_, st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	run, err := st.GetRun(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("run %s not found (gpn status lists runs)", fs.Arg(0))
	}
	switch run.Status {
	case "done", "failed", "stopped":
		return fmt.Errorf("run %s is already %s", run.ID, run.Status)
	}
	if err := engine.RequestStop(filepath.Join(c.root, ".gpn"), run.ID); err != nil {
		return err
	}
	(&audit.Chain{S: st}).Append(run.ID, "", "stop_requested", nil)
	fmt.Println("stop requested for", run.ID)
	return nil
}

// ---- trace / audit -------------------------------------------------------

func cmdTrace(args []string) error {
	fs := flag.NewFlagSet("trace", flag.ExitOnError)
	c := bindCommon(fs)
	parse(fs, args)
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: gpn trace why <file>:<line> | coverage | orphans | impact <node> | blast <file>")
	}
	if err := c.resolve(); err != nil {
		return err
	}
	_, st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	g := &trace.Graph{S: st}
	var rows []trace.Row
	switch fs.Arg(0) {
	case "why":
		if fs.NArg() < 2 {
			return fmt.Errorf("usage: gpn trace why <file>:<line>")
		}
		file, line := splitFileLine(fs.Arg(1))
		rows, err = g.Query("why", map[string]string{"repo": c.root, "file": file, "line": strconv.Itoa(line)})
	case "coverage":
		rows, err = g.Query("coverage_gap", nil)
	case "orphans":
		rows, err = g.Query("orphans", nil)
	case "impact":
		rows, err = g.Query("impact", map[string]string{"node": fs.Arg(1)})
	case "blast":
		rows, err = g.Query("blast_radius", map[string]string{"file": fs.Arg(1)})
	default:
		return fmt.Errorf("unknown trace query %q", fs.Arg(0))
	}
	if err != nil {
		return err
	}
	if rows == nil {
		rows = []trace.Row{}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}

func splitFileLine(s string) (string, int) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return s, 0
	}
	n, _ := strconv.Atoi(s[i+1:])
	return s[:i], n
}

func cmdAudit(args []string) error {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	c := bindCommon(fs)
	parse(fs, args)
	if fs.NArg() < 1 || fs.Arg(0) != "verify" {
		return fmt.Errorf("usage: gpn audit verify")
	}
	if err := c.resolve(); err != nil {
		return err
	}
	_, st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	n, err := (&audit.Chain{S: st}).Verify()
	if err != nil {
		return err
	}
	fmt.Printf("audit chain intact: %d entries\n", n)
	return nil
}

// ---- registry / eval -----------------------------------------------------

func cmdRegistry(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: gpn registry keygen|publish|install|verify|list")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("registry "+sub, flag.ExitOnError)
	c := bindCommon(fs)
	keyID := fs.String("key", "local", "signing key id")
	signer := fs.String("signer", os.Getenv("USER"), "signer name recorded in the signature")
	parse(fs, rest)
	if err := c.resolve(); err != nil {
		return err
	}
	switch sub {
	case "keygen":
		id := *keyID
		if fs.NArg() > 0 {
			id = fs.Arg(0)
		}
		if _, err := registry.GenerateKeys(keysDir(), id); err != nil {
			return err
		}
		fmt.Println("generated", filepath.Join(keysDir(), id+".{key,pub}"))
		return nil
	case "publish":
		if fs.NArg() < 1 {
			return fmt.Errorf("usage: gpn registry publish <plugin-dir>")
		}
		keys, err := registry.LoadKeys(keysDir(), *keyID)
		if err != nil {
			return err
		}
		m, sig, err := registry.Publish(fs.Arg(0), keys, *signer)
		if err != nil {
			return err
		}
		fmt.Printf("published %s@%s tier=%s digest=%s key=%s\n", m.Name, m.Version, m.Tier, m.Digest, sig.KeyID)
		return nil
	case "install":
		if fs.NArg() < 1 {
			return fmt.Errorf("usage: gpn registry install <plugin-dir>")
		}
		p, st, err := c.open()
		if err != nil {
			return err
		}
		defer st.Close()
		keys, _ := registry.LoadKeys(keysDir(), *keyID)
		m, err := registry.Install(st, fs.Arg(0), keys, p.Registry.TiersAllowed, p.Registry.RequireSignature)
		if err != nil {
			return err
		}
		(&audit.Chain{S: st}).Append("", "", "skill_installed", map[string]any{"name": m.Name, "version": m.Version, "tier": m.Tier, "digest": m.Digest})
		fmt.Printf("installed %s@%s (tier %s, verified)\n", m.Name, m.Version, m.Tier)
		return nil
	case "verify":
		_, st, err := c.open()
		if err != nil {
			return err
		}
		defer st.Close()
		keys, _ := registry.LoadKeys(keysDir(), *keyID)
		skills, err := st.ListSkills()
		if err != nil {
			return err
		}
		bad := 0
		for _, sk := range skills {
			if _, _, err := registry.Verify(sk.Path, keys); err != nil {
				bad++
				fmt.Printf("FAIL %s@%s: %v\n", sk.Name, sk.Version, err)
			} else {
				fmt.Printf("ok   %s@%s %s\n", sk.Name, sk.Version, sk.Digest)
			}
		}
		if bad > 0 {
			return fmt.Errorf("%d installed skill(s) failed verification", bad)
		}
		return nil
	case "list":
		_, st, err := c.open()
		if err != nil {
			return err
		}
		defer st.Close()
		skills, err := st.ListSkills()
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tVERSION\tTIER\tOWNER\tDIGEST\tPATH")
		for _, sk := range skills {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", sk.Name, sk.Version, sk.Tier, sk.Owner, short(sk.Digest), sk.Path)
		}
		return w.Flush()
	}
	return fmt.Errorf("unknown registry command %q", sub)
}

func short(s string) string {
	if len(s) > 19 {
		return s[:19] + "…"
	}
	return s
}

func cmdEval(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	c := bindCommon(fs)
	judge := fs.String("judge-model", "", "judge model (default: manifest)")
	runs := fs.Int("runs", 0, "runs per case (default: manifest)")
	threshold := fs.Float64("threshold", 0, "pass threshold 0..1 (default: manifest)")
	maxCost := fs.Float64("max-cost", 5, "cost ceiling in USD")
	dry := fs.Bool("dry-run", false, "print the claude command and exit")
	parse(fs, args)
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: gpn eval <plugin-dir>")
	}
	if err := c.resolve(); err != nil {
		return err
	}
	m, err := registry.LoadManifest(fs.Arg(0))
	if err != nil {
		return err
	}
	o := evals.Options{PluginDir: fs.Arg(0), JudgeModel: *judge, Runs: *runs, Threshold: *threshold, MaxCostUSD: *maxCost, TrustPlugin: true}
	if *dry {
		if o.JudgeModel == "" {
			o.JudgeModel = m.Evals.JudgeModel
		}
		if o.Runs == 0 {
			o.Runs = m.Evals.Runs
		}
		if o.Threshold == 0 {
			o.Threshold = m.Evals.Threshold
		}
		fmt.Println(strings.Join(evals.Command(o, "<results.json>"), " "))
		return nil
	}
	_, st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()
	rep, err := evals.Run(ctx, st, m, o)
	fmt.Printf("eval report %s archived (pass rate %.2f, threshold %.2f)\n", rep.ID, rep.PassRate, rep.Threshold)
	return err
}

// ---- policy --------------------------------------------------------------

func cmdPolicy(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: gpn policy compile | check <tool> '<json>'")
	}
	fs := flag.NewFlagSet("policy", flag.ExitOnError)
	c := bindCommon(fs)
	parse(fs, args[1:])
	if err := c.resolve(); err != nil {
		return err
	}
	switch args[0] {
	case "compile":
		p, st, err := c.open()
		if err != nil {
			return err
		}
		defer st.Close()
		exe, _ := os.Executable()
		eng, err := engine.New(engine.Config{Root: c.root, PolicyPath: c.policyPath, Policy: p, Store: st, GpnBinary: exe})
		if err != nil {
			return err
		}
		fmt.Println("wrote", eng.SettingsPath(), "and", filepath.Join(filepath.Dir(eng.SettingsPath()), "mcp.json"))
		fmt.Println("policy hash", p.Hash())
		return nil
	case "check":
		if fs.NArg() < 2 {
			return fmt.Errorf("usage: gpn policy check <tool> '<json tool_input>'")
		}
		p, err := policy.Load(c.policyPath)
		if err != nil {
			return err
		}
		var input map[string]any
		if err := json.Unmarshal([]byte(fs.Arg(1)), &input); err != nil {
			return fmt.Errorf("tool_input must be JSON: %w", err)
		}
		d := p.Decide(fs.Arg(0), input)
		b, _ := json.MarshalIndent(d, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	return fmt.Errorf("unknown policy command %q", args[0])
}

// ---- hook / mcp / internal ----------------------------------------------

func cmdHook(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: gpn hook pre|permission|post|stop --policy P --db D")
		return 2
	}
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	c := bindCommon(fs)
	wait := fs.Duration("wait", 25*time.Minute, "how long a permission hook waits for a human")
	if err := parse(fs, args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := c.resolve(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	p, err := policy.Load(c.policyPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gpn hook: cannot load policy, blocking:", err)
		return 2
	}
	st, err := store.Open(c.dbPath)
	if err != nil {
		st = nil
	} else {
		defer st.Close()
	}
	env := hook.EnvFromProcess()
	switch args[0] {
	case "pre":
		return hook.Pre(os.Stdin, os.Stdout, os.Stderr, p, st, env)
	case "permission":
		return hook.Permission(ctx, os.Stdin, os.Stdout, os.Stderr, p, st, env, *wait, 2*time.Second)
	case "post":
		return hook.Post(os.Stdin, os.Stdout, os.Stderr, st, env)
	case "stop":
		return hook.Stop(os.Stdin, os.Stdout, os.Stderr, st, env)
	}
	fmt.Fprintln(os.Stderr, "unknown hook", args[0])
	return 2
}

func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	c := bindCommon(fs)
	parse(fs, args)
	if err := c.resolve(); err != nil {
		return err
	}
	st, err := store.Open(c.dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	srv := &mcp.Server{Store: st, Repo: c.root, Version: version}
	return srv.ServeStdio()
}

func cmdInternal(args []string) error {
	if len(args) < 1 || args[0] != "merge" {
		return fmt.Errorf("usage: gpn internal merge --run <run-id>")
	}
	fs := flag.NewFlagSet("internal merge", flag.ExitOnError)
	c := bindCommon(fs)
	runID := fs.String("run", "", "run id")
	parse(fs, args[1:])
	if err := c.resolve(); err != nil {
		return err
	}
	if *runID == "" {
		return fmt.Errorf("--run is required")
	}
	// The engine performs merges in-process; this entry point exists so a
	// workflow's shell step reads the same whether run by gpn or by hand.
	fmt.Println("merge is performed by the engine's shell step for run", *runID)
	return nil
}

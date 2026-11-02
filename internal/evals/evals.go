// Package evals wraps `claude plugin eval` so every skill ships with CI eval
// coverage: pinned judge model, fixed run count, threshold, cost ceiling, and
// an archived JSON report in the store.
package evals

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gpn/gpn/internal/registry"
	"github.com/gpn/gpn/internal/store"
)

// Options control one eval run.
type Options struct {
	ClaudeBinary string
	PluginDir    string
	JudgeModel   string
	Runs         int
	Threshold    float64
	MaxCostUSD   float64
	TrustPlugin  bool
}

// Command returns the exact command line that would run, for dry runs.
func Command(o Options, jsonOut string) []string {
	bin := o.ClaudeBinary
	if bin == "" {
		bin = "claude"
	}
	args := []string{bin, "plugin", "eval", o.PluginDir, "--json", jsonOut, "--no-publish", "--ablation", "with-without"}
	if o.JudgeModel != "" {
		args = append(args, "--judge-model", o.JudgeModel)
	}
	if o.Runs > 0 {
		args = append(args, "--runs", strconv.Itoa(o.Runs))
	}
	if o.Threshold > 0 {
		args = append(args, "--threshold", strconv.FormatFloat(o.Threshold, 'f', 2, 64))
	}
	if o.MaxCostUSD > 0 {
		args = append(args, "--max-cost-usd", strconv.FormatFloat(o.MaxCostUSD, 'f', 2, 64))
	}
	if o.TrustPlugin {
		args = append(args, "--trust-plugin")
	}
	return args
}

// Run executes the eval, archives the report and returns it. A non-zero exit
// from claude (threshold miss or cost ceiling) is returned as an error after
// the report is archived, so the gate fails but the evidence is kept.
func Run(ctx context.Context, st *store.Store, m *registry.Manifest, o Options) (store.EvalReport, error) {
	if o.JudgeModel == "" {
		o.JudgeModel = m.Evals.JudgeModel
	}
	if o.Runs == 0 {
		o.Runs = m.Evals.Runs
	}
	if o.Threshold == 0 {
		o.Threshold = m.Evals.Threshold
	}
	outDir := filepath.Join(o.PluginDir, "evals", "results")
	_ = os.MkdirAll(outDir, 0o755)
	jsonOut := filepath.Join(outDir, fmt.Sprintf("gpn-%d.json", time.Now().Unix()))
	argv := Command(o, jsonOut)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var combined bytes.Buffer
	cmd.Stdout, cmd.Stderr = &combined, &combined
	runErr := cmd.Run()

	rep := store.EvalReport{ID: fmt.Sprintf("EVAL-%s-%s-%d", m.Name, m.Version, time.Now().Unix()), Skill: m.Name, Version: m.Version, JudgeModel: o.JudgeModel, Runs: o.Runs, Threshold: o.Threshold}
	if b, err := os.ReadFile(jsonOut); err == nil {
		rep.ResultsJSON = string(b)
		rep.PassRate, rep.CostUSD = summarize(b)
	} else {
		rep.ResultsJSON = combined.String()
	}
	if err := st.InsertEvalReport(rep); err != nil {
		return rep, err
	}
	if runErr != nil {
		return rep, fmt.Errorf("eval gate failed for %s@%s: %w (report %s)", m.Name, m.Version, runErr, rep.ID)
	}
	return rep, nil
}

// summarize pulls a pass rate and cost out of the eval JSON when the shape
// is recognizable; otherwise returns zeros. The report itself is kept whole.
func summarize(b []byte) (passRate, cost float64) {
	var any map[string]any
	if err := json.Unmarshal(b, &any); err != nil {
		return 0, 0
	}
	for _, k := range []string{"score", "pass_rate", "passRate", "mean_score"} {
		if v, ok := any[k].(float64); ok {
			passRate = v
			break
		}
	}
	for _, k := range []string{"total_cost_usd", "cost_usd", "totalCostUsd"} {
		if v, ok := any[k].(float64); ok {
			cost = v
			break
		}
	}
	return passRate, cost
}

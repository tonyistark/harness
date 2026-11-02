package claudecode

import (
	"strings"
	"testing"

	"github.com/gpn/gpn/internal/driver"
)

func TestParseLines(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"s1","model":"claude-x","plugins":[{"name":"sdd-core","version":"0.1.0"}]}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Working"},{"type":"tool_use","id":"toolu_1","name":"Write","input":{"file_path":"a.md","content":"x"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"{\"requirements\":[]}","session_id":"s1","total_cost_usd":0.012,"usage":{"input_tokens":100,"output_tokens":20},"modelUsage":{"claude-x":{}},"permission_denials":[]}`,
	}
	var types []driver.EventType
	var res *driver.Result
	for _, l := range lines {
		evs, r, err := ParseLine([]byte(l), 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			types = append(types, e.Type)
		}
		if r != nil {
			res = r
		}
	}
	want := []driver.EventType{driver.EvSystem, driver.EvText, driver.EvToolCall, driver.EvToolResult, driver.EvResult}
	if len(types) != len(want) {
		t.Fatalf("types %v", types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("event %d = %s want %s", i, types[i], want[i])
		}
	}
	if res == nil || res.SessionID != "s1" || res.VendorCostUSD != 0.012 || res.ModelResponded != "claude-x" || res.Usage.InputTokens != 100 {
		t.Fatalf("result %+v", res)
	}
	if string(res.StructuredOutput) != `{"requirements":[]}` {
		t.Fatalf("structured output not recovered from result text: %s", res.StructuredOutput)
	}
}

func TestArgsAreBareAndGoverned(t *testing.T) {
	d := New()
	args := d.Args(driver.StepSpec{Prompt: "hi", SettingsPath: "/s.json", MCPConfigPath: "/m.json", Model: "m", FallbackModel: "f", BudgetUSD: 1.5, PluginDirs: []string{"/p"}})
	joined := strings.Join(args, " ")
	for _, must := range []string{"--bare", "--output-format stream-json", "--settings /s.json", "--mcp-config /m.json", "--model m", "--fallback-model f", "--max-budget-usd 1.50", "--plugin-dir /p"} {
		if !strings.Contains(joined, must) {
			t.Errorf("args missing %q: %s", must, joined)
		}
	}
	if strings.Contains(joined, "skip-permissions") {
		t.Errorf("args must never contain skip-permissions: %s", joined)
	}
}

func TestArgsOmitUnsupportedFlags(t *testing.T) {
	d := New()
	var warnings []string
	d.Logf = func(f string, a ...any) { warnings = append(warnings, f) }
	d.SetCapabilities("2.0.70", map[string]bool{"--settings": true, "--mcp-config": true, "--model": true, "--output-format": true, "--verbose": true, "--allowedTools": true})
	args := d.Args(driver.StepSpec{Prompt: "hi", SettingsPath: "/s.json", Model: "m", FallbackModel: "f", BudgetUSD: 1, PluginDirs: []string{"/p"}})
	joined := strings.Join(args, " ")
	for _, mustNot := range []string{"--bare", "--fallback-model", "--max-budget-usd", "--plugin-dir"} {
		if strings.Contains(joined, mustNot) {
			t.Errorf("unsupported flag %s was passed: %s", mustNot, joined)
		}
	}
	if !strings.Contains(joined, "--settings /s.json") || !strings.Contains(joined, "--model m") {
		t.Errorf("supported flags missing: %s", joined)
	}
	if len(warnings) < 4 {
		t.Errorf("expected warnings for each omitted flag, got %d", len(warnings))
	}
}

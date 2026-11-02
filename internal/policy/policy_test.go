package policy

import (
	"encoding/json"
	"testing"

	"github.com/gpn/gpn/internal/assets"
)

func seed(t *testing.T) *Policy {
	t.Helper()
	b, err := assets.Read("gpn-policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDecide(t *testing.T) {
	p := seed(t)
	cases := []struct {
		tool  string
		input map[string]any
		want  string
	}{
		{"Bash", map[string]any{"command": "git push --force origin main"}, "deny"},
		{"Bash", map[string]any{"command": "rm -rf /tmp/x"}, "deny"},
		{"Bash", map[string]any{"command": "git push origin feature"}, "ask"},
		{"Bash", map[string]any{"command": "go test ./..."}, "allow"},
		{"Write", map[string]any{"file_path": ".git/hooks/pre-commit"}, "deny"},
		{"Edit", map[string]any{"file_path": "/repo/.gpn/wt/RUN-1/T1/.claude/settings.json"}, "deny"},
		{"Edit", map[string]any{"file_path": "deploy/prod.yaml"}, "ask"},
		{"Edit", map[string]any{"file_path": "migrations/0007_add.sql"}, "ask"},
		{"Edit", map[string]any{"file_path": "internal/ingest/ingest.go"}, "allow"},
		{"WebFetch", map[string]any{"url": "https://example.com"}, "deny"},
		{"mcp__github__create_pull_request", map[string]any{}, "ask"},
		{"mcp__github__delete_repo", map[string]any{}, "deny"},
		{"Bash", map[string]any{"command": "echo hi > .git/hooks/post-commit"}, "deny"},
	}
	for _, c := range cases {
		d := p.Decide(c.tool, c.input)
		if d.Behavior != c.want {
			t.Errorf("%s %v: got %s (%s), want %s", c.tool, c.input, d.Behavior, d.Reason, c.want)
		}
	}
}

func TestCompileSettings(t *testing.T) {
	p := seed(t)
	b, err := p.CompileSettings(CompileOptions{GpnBinary: "/usr/local/bin/gpn", PolicyPath: "/r/gpn-policy.yaml", DBPath: "/r/.gpn/gpn.db"})
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	sb := s["sandbox"].(map[string]any)
	if sb["failIfUnavailable"] != true || sb["allowUnsandboxedCommands"] != false {
		t.Fatalf("sandbox not fail-closed: %v", sb)
	}
	hooks := s["hooks"].(map[string]any)
	for _, ev := range []string{"PreToolUse", "PermissionRequest", "PostToolUse", "Stop"} {
		if _, ok := hooks[ev]; !ok {
			t.Errorf("missing hook %s", ev)
		}
	}
	pre := hooks["PreToolUse"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if pre["onFailure"] != "block" {
		t.Errorf("PreToolUse hook must block on failure")
	}
	perms := s["permissions"].(map[string]any)
	if perms["disableBypassPermissionsMode"] != "disable" {
		t.Errorf("bypass mode must be disabled")
	}
}

func TestResolveModel(t *testing.T) {
	p := seed(t)
	cat := &Catalog{Models: map[string]CatalogEntry{
		"opus":   {Route: map[string]string{"bedrock": "b-opus", "anthropic": "a-opus"}, ClearedClasses: []string{"design", "review"}},
		"sonnet": {Route: map[string]string{"bedrock": "b-sonnet", "anthropic": "a-sonnet"}, ClearedClasses: []string{"*"}},
		"haiku":  {Route: map[string]string{"bedrock": "b-haiku"}, ClearedClasses: []string{"mechanical", "implement"}},
	}}
	r, err := p.ResolveModel("design", cat, "")
	if err != nil || r.Model != "b-opus" || r.Fallback != "b-sonnet" || r.Tier != "opus" {
		t.Fatalf("design: %+v %v", r, err)
	}
	r, err = p.ResolveModel("implement", cat, "anthropic")
	if err != nil || r.Model != "a-sonnet" || r.Route != "anthropic" {
		t.Fatalf("implement on anthropic: %+v %v", r, err)
	}
	if _, err := p.ResolveModel("mechanical", cat, "anthropic"); err == nil {
		t.Fatal("haiku has no anthropic route; expected error")
	}
}

func TestCheckRuntimeVersion(t *testing.T) {
	p := &Policy{ClaudeCodeVersion: ">=2.1.300"}
	if err := p.CheckRuntimeVersion("2.1.316 (Claude Code)"); err != nil {
		t.Fatalf("2.1.316 should satisfy >=2.1.300: %v", err)
	}
	if err := p.CheckRuntimeVersion("2.0.70"); err == nil {
		t.Fatal("2.0.70 should fail >=2.1.300")
	}
	if err := p.CheckRuntimeVersion("2.1.300"); err != nil {
		t.Fatalf("equal version should pass: %v", err)
	}
	p.ClaudeCodeVersion = ""
	if err := p.CheckRuntimeVersion("1.0.0"); err != nil {
		t.Fatal("empty constraint must not fail")
	}
}

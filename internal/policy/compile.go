package policy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// CompileOptions tell the compiler where the gpn binary, the policy and the
// database live so hooks can find them when Claude Code invokes them.
type CompileOptions struct {
	GpnBinary         string
	PolicyPath        string
	DBPath            string
	HookTimeoutSec    int
	PermissionWaitSec int
}

// CompileSettings produces the Claude Code settings JSON passed with
// --settings. Every rule in the policy appears here, and every policy hook
// points back at the gpn binary with onFailure: block.
func (p *Policy) CompileSettings(o CompileOptions) ([]byte, error) {
	if o.GpnBinary == "" {
		o.GpnBinary = "gpn"
	}
	if o.HookTimeoutSec == 0 {
		o.HookTimeoutSec = 30
	}
	if o.PermissionWaitSec == 0 {
		o.PermissionWaitSec = 1800
	}
	perms := map[string]any{}
	allow := append([]string{}, p.Tools.Allow...)
	deny := append([]string{}, p.Tools.Deny...)
	ask := append([]string{}, p.Tools.Approve...)
	for _, c := range p.Commands.Deny {
		deny = append(deny, "Bash("+c+")")
	}
	for _, c := range p.Commands.Approve {
		ask = append(ask, "Bash("+c+")")
	}
	for _, pp := range p.ProtectedPaths {
		for _, t := range []string{"Edit", "Write", "MultiEdit", "NotebookEdit"} {
			deny = append(deny, t+"("+pp+")")
		}
	}
	deny = append(deny, "Bash(claude *)") // no nested sessions outside the engine
	perms["allow"] = allow
	perms["deny"] = deny
	perms["ask"] = ask
	switch p.Autonomy {
	case "supervised":
		perms["defaultMode"] = "acceptEdits"
	default:
		perms["defaultMode"] = "default"
	}
	perms["disableBypassPermissionsMode"] = "disable"

	sandbox := map[string]any{
		"enabled":                  true,
		"failIfUnavailable":        p.Sandbox.Required,
		"allowUnsandboxedCommands": false,
		"network":                  map[string]any{"allowedDomains": nonNil(p.Sandbox.NetworkAllow)},
		"filesystem":               map[string]any{"denyWrite": nonNil(p.ProtectedPaths)},
	}

	common := []string{"--policy", o.PolicyPath, "--db", o.DBPath}
	hook := func(sub string, timeout int) map[string]any {
		return map[string]any{
			"type":      "command",
			"command":   o.GpnBinary,
			"args":      append([]string{"hook", sub}, common...),
			"timeout":   timeout,
			"onFailure": "block",
		}
	}
	hooks := map[string]any{
		"PreToolUse":        []any{map[string]any{"matcher": "Bash|Edit|Write|MultiEdit|NotebookEdit", "hooks": []any{hook("pre", o.HookTimeoutSec)}}},
		"PermissionRequest": []any{map[string]any{"matcher": "*", "hooks": []any{hook("permission", o.PermissionWaitSec)}}},
		"PostToolUse":       []any{map[string]any{"matcher": "*", "hooks": []any{hook("post", o.HookTimeoutSec)}}},
		"Stop":              []any{map[string]any{"matcher": "*", "hooks": []any{hook("stop", o.HookTimeoutSec)}}},
	}

	settings := map[string]any{
		"permissions": perms,
		"sandbox":     sandbox,
		"hooks":       hooks,
		"gpn":         map[string]any{"policy_hash": p.Hash(), "autonomy": p.Autonomy},
	}
	return json.MarshalIndent(settings, "", "  ")
}

// MCPServer describes one MCP server to register for a session.
type MCPServer struct {
	Command string
	Args    []string
	Env     map[string]string
}

// CompileMCPConfig produces the --mcp-config JSON: the gpn broker plus any
// extra servers the caller supplies (connectors chosen from the registry).
func (p *Policy) CompileMCPConfig(o CompileOptions, extra map[string]MCPServer) ([]byte, error) {
	if o.GpnBinary == "" {
		o.GpnBinary = "gpn"
	}
	servers := map[string]any{
		"gpn": map[string]any{"type": "stdio", "command": o.GpnBinary, "args": []string{"mcp", "--policy", o.PolicyPath, "--db", o.DBPath}},
	}
	for name, s := range extra {
		if name == "gpn" {
			return nil, fmt.Errorf("mcp server name %q is reserved", name)
		}
		entry := map[string]any{"type": "stdio", "command": s.Command, "args": s.Args}
		if len(s.Env) > 0 {
			entry["env"] = s.Env
		}
		servers[name] = entry
	}
	return json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
}

// PermissionMode returns the --permission-mode flag value for the autonomy.
func (p *Policy) PermissionMode() string {
	switch p.Autonomy {
	case "supervised":
		return "acceptEdits"
	case "autonomous":
		return "acceptEdits"
	default:
		return ""
	}
}

// GateCommand returns the shell command configured for a gate check name such
// as "build" or "coverage>=80" (threshold stripped), or an error.
func (p *Policy) GateCommand(check string) (name, cmd string, err error) {
	name = check
	if i := strings.IndexAny(check, "<>="); i > 0 {
		name = check[:i]
	}
	cmd, ok := p.Gates[name]
	if !ok {
		return name, "", fmt.Errorf("policy: no gates.%s command configured", name)
	}
	return name, cmd, nil
}

func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

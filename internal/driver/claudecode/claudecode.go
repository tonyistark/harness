// Package claudecode drives Claude Code headless through its stream-json
// protocol: `claude -p --bare --output-format stream-json --verbose ...`.
//
// The driver never reads the user's ~/.claude or the repository's own
// settings: every session runs in bare mode with the policy-compiled settings,
// MCP config and verified plugin directories passed explicitly.
package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gpn/gpn/internal/driver"
)

// Driver runs the claude binary.
type Driver struct {
	Binary    string
	ExtraArgs []string
	// Logf receives warnings about the installed Claude Code (missing flags,
	// old version). Nil discards them.
	Logf func(format string, args ...any)

	probeOnce sync.Once
	caps      map[string]bool // flag -> supported; nil when the probe failed
	version   string
	probeErr  error
}

// New returns a driver that uses `claude` from PATH.
func New() *Driver { return &Driver{Binary: "claude"} }

// Name implements driver.Driver.
func (d *Driver) Name() string { return "claude-code" }

// flags the driver may pass; each is probed against `claude --help` so an
// older Claude Code gets a working command line and a warning rather than
// "unknown option".
var probedFlags = []string{"--bare", "--settings", "--mcp-config", "--model", "--fallback-model", "--max-budget-usd", "--json-schema", "--append-system-prompt-file", "--permission-mode", "--allowedTools", "--plugin-dir", "--verbose", "--output-format"}

func (d *Driver) probe() {
	d.probeOnce.Do(func() {
		if d.caps != nil {
			return // preset by a test
		}
		out, err := exec.Command(d.Binary, "--version").Output()
		if err != nil {
			d.probeErr = fmt.Errorf("claude: cannot run %q: %w", d.Binary, err)
			return
		}
		d.version = strings.Fields(strings.TrimSpace(string(out)))[0]
		help, err := exec.Command(d.Binary, "--help").CombinedOutput()
		if err != nil {
			d.probeErr = fmt.Errorf("claude --help: %w", err)
			return
		}
		d.caps = map[string]bool{}
		for _, f := range probedFlags {
			d.caps[f] = bytes.Contains(help, []byte(f))
		}
	})
}

// SetCapabilities presets the probe result (tests).
func (d *Driver) SetCapabilities(version string, caps map[string]bool) {
	d.probeOnce.Do(func() {})
	d.version, d.caps = version, caps
}

// Version returns the installed Claude Code version ("2.1.316").
func (d *Driver) Version() (string, error) {
	d.probe()
	if d.probeErr != nil {
		return "", d.probeErr
	}
	return d.version, nil
}

// Supports reports whether the installed Claude Code accepts a flag. Unknown
// (probe failed) counts as supported so the real error surfaces from claude.
func (d *Driver) Supports(flag string) bool {
	d.probe()
	if d.caps == nil {
		return true
	}
	return d.caps[flag]
}

func (d *Driver) logf(format string, args ...any) {
	if d.Logf != nil {
		d.Logf(format, args...)
	}
}

// Args builds the command line for a step, omitting flags the installed
// Claude Code does not support (with a warning). Exported so tests and
// `gpn run --dry-run` can show exactly what would be executed.
func (d *Driver) Args(spec driver.StepSpec) []string {
	args := []string{"-p", spec.Prompt}
	if d.Supports("--bare") {
		args = append(args, "--bare")
	} else {
		d.logf("claude %s has no --bare: user and project settings, hooks and MCP servers may load alongside the policy; upgrade Claude Code (npm i -g @anthropic-ai/claude-code@latest)", d.version)
	}
	args = append(args, "--output-format", "stream-json", "--verbose")
	add := func(flag string, values ...string) {
		if !d.Supports(flag) {
			d.logf("claude %s has no %s; continuing without it", d.version, flag)
			return
		}
		args = append(args, append([]string{flag}, values...)...)
	}
	if spec.SettingsPath != "" {
		add("--settings", spec.SettingsPath)
	}
	if spec.MCPConfigPath != "" {
		add("--mcp-config", spec.MCPConfigPath)
	}
	if spec.Model != "" {
		add("--model", spec.Model)
	}
	if spec.FallbackModel != "" {
		add("--fallback-model", spec.FallbackModel)
	}
	if spec.BudgetUSD > 0 {
		add("--max-budget-usd", strconv.FormatFloat(spec.BudgetUSD, 'f', 2, 64))
	}
	if spec.OutputSchemaPath != "" && os.Getenv("GPN_NO_JSON_SCHEMA") == "" {
		if b, err := os.ReadFile(spec.OutputSchemaPath); err == nil {
			add("--json-schema", strings.TrimSpace(string(b)))
		}
	}
	if spec.SystemPromptFile != "" {
		add("--append-system-prompt-file", spec.SystemPromptFile)
	}
	if spec.PermissionMode != "" {
		add("--permission-mode", spec.PermissionMode)
	}
	if len(spec.AllowedTools) > 0 {
		add("--allowedTools", strings.Join(spec.AllowedTools, ","))
	}
	for _, p := range spec.PluginDirs {
		add("--plugin-dir", p)
	}
	return append(args, d.ExtraArgs...)
}

// Start implements driver.Driver.
func (d *Driver) Start(ctx context.Context, spec driver.StepSpec) (driver.Session, error) {
	cmd := exec.CommandContext(ctx, d.Binary, d.Args(spec)...)
	cmd.Dir = spec.Cwd
	env := os.Environ()
	keys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+spec.Env[k])
	}
	env = append(env, "GPN_RUN_ID="+spec.RunID, "GPN_STEP_ID="+spec.StepID)
	cmd.Env = env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	s := &session{cmd: cmd, events: make(chan driver.Event, 256), done: make(chan struct{})}
	cmd.Stderr = &s.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("claude: start: %w", err)
	}
	go s.pump(stdout)
	return s, nil
}

type session struct {
	cmd    *exec.Cmd
	events chan driver.Event
	done   chan struct{}
	stderr bytes.Buffer
	mu     sync.Mutex
	result driver.Result
	err    error
	seq    int
}

func (s *session) Events() <-chan driver.Event { return s.events }

func (s *session) Wait() (driver.Result, error) {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result, s.err
}

func (s *session) Interrupt() error {
	if s.cmd.Process == nil {
		return nil
	}
	return s.cmd.Process.Signal(os.Interrupt)
}

func (s *session) pump(r interface{ Read([]byte) (int, error) }) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	var res *driver.Result
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		evs, r2, perr := ParseLine(line, s.seq)
		if perr != nil {
			s.seq++
			s.events <- driver.Event{Type: driver.EvError, Seq: s.seq, Time: time.Now(), Text: perr.Error(), Payload: json.RawMessage(append([]byte(nil), line...))}
			continue
		}
		for _, ev := range evs {
			s.seq++
			ev.Seq = s.seq
			s.events <- ev
		}
		if r2 != nil {
			res = r2
		}
	}
	waitErr := s.cmd.Wait()
	s.mu.Lock()
	if res != nil {
		s.result = *res
	}
	if waitErr != nil {
		s.err = fmt.Errorf("claude exited: %w: %s", waitErr, strings.TrimSpace(s.stderr.String()))
	} else if res == nil {
		s.err = fmt.Errorf("claude produced no result event: %s", strings.TrimSpace(s.stderr.String()))
	}
	s.mu.Unlock()
	close(s.events)
	close(s.done)
}

// stream-json shapes we care about. Fields not listed are ignored.
type rawMessage struct {
	Type            string          `json:"type"`
	Subtype         string          `json:"subtype"`
	SessionID       string          `json:"session_id"`
	ParentToolUseID *string         `json:"parent_tool_use_id"`
	Message         *rawContent     `json:"message"`
	Model           string          `json:"model"`
	Plugins         []rawPlugin     `json:"plugins"`
	IsError         bool            `json:"is_error"`
	Result          string          `json:"result"`
	TotalCostUSD    float64         `json:"total_cost_usd"`
	Usage           *driver.Usage   `json:"usage"`
	ModelUsage      map[string]any  `json:"modelUsage"`
	Structured      json.RawMessage `json:"structured_output"`
	Denials         []any           `json:"permission_denials"`
}

type rawPlugin struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Path    string `json:"path"`
}

type rawContent struct {
	Role    string     `json:"role"`
	Model   string     `json:"model"`
	Content []rawBlock `json:"content"`
}

type rawBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// ParseLine turns one stream-json line into normalized events and, for the
// final line, a Result. seq is the caller's running counter (informational).
func ParseLine(line []byte, seq int) ([]driver.Event, *driver.Result, error) {
	var m rawMessage
	if err := json.Unmarshal(line, &m); err != nil {
		return nil, nil, fmt.Errorf("stream-json: %w", err)
	}
	now := time.Now()
	raw := json.RawMessage(append([]byte(nil), line...))
	switch m.Type {
	case "system":
		return []driver.Event{{Type: driver.EvSystem, Time: now, Text: m.Subtype, Payload: raw}}, nil, nil
	case "assistant":
		var evs []driver.Event
		if m.Message != nil {
			for _, b := range m.Message.Content {
				switch b.Type {
				case "text":
					if strings.TrimSpace(b.Text) != "" {
						evs = append(evs, driver.Event{Type: driver.EvText, Time: now, Text: b.Text})
					}
				case "tool_use":
					evs = append(evs, driver.Event{Type: driver.EvToolCall, Time: now, ToolName: b.Name, ToolUseID: b.ID, Input: b.Input})
				}
			}
		}
		return evs, nil, nil
	case "user":
		var evs []driver.Event
		if m.Message != nil {
			for _, b := range m.Message.Content {
				if b.Type == "tool_result" {
					evs = append(evs, driver.Event{Type: driver.EvToolResult, Time: now, ToolUseID: b.ToolUseID, Output: b.Content, IsError: b.IsError})
				}
			}
		}
		return evs, nil, nil
	case "result":
		res := &driver.Result{SessionID: m.SessionID, VendorCostUSD: m.TotalCostUSD, ResultText: m.Result, StructuredOutput: m.Structured, IsError: m.IsError || m.Subtype != "success", PermissionDenials: len(m.Denials), Raw: raw}
		if m.Usage != nil {
			res.Usage = *m.Usage
		}
		if len(m.ModelUsage) > 0 {
			names := make([]string, 0, len(m.ModelUsage))
			for k := range m.ModelUsage {
				names = append(names, k)
			}
			sort.Strings(names)
			res.ModelResponded = strings.Join(names, ",")
		}
		if len(res.StructuredOutput) == 0 && looksLikeJSON(m.Result) {
			res.StructuredOutput = json.RawMessage(m.Result)
		}
		return []driver.Event{{Type: driver.EvResult, Time: now, Payload: raw}}, res, nil
	default:
		return []driver.Event{{Type: driver.EvSystem, Time: now, Text: m.Type, Payload: raw}}, nil, nil
	}
}

// PluginVersions extracts "name@version" from a system/init payload.
func PluginVersions(initPayload json.RawMessage) []string {
	var m rawMessage
	if err := json.Unmarshal(initPayload, &m); err != nil {
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

func looksLikeJSON(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return false
	}
	if (s[0] == '{' && s[len(s)-1] == '}') || (s[0] == '[' && s[len(s)-1] == ']') {
		return json.Valid([]byte(s))
	}
	return false
}

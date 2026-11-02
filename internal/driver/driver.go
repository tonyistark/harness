// Package driver defines the interface between the engine and an agent CLI.
//
// Only one implementation ships in the prototype (Claude Code, headless), but
// the interface is published so a second CLI can be added without touching
// workflows: a driver turns a StepSpec into a Session and a Session yields a
// normalized stream of Events and one Result.
package driver

import (
	"context"
	"encoding/json"
	"time"
)

// StepSpec is everything a driver needs to run one agent step.
type StepSpec struct {
	RunID            string
	StepID           string
	Prompt           string
	SystemPromptFile string
	Model            string
	FallbackModel    string
	BudgetUSD        float64
	OutputSchemaPath string
	SettingsPath     string
	MCPConfigPath    string
	PluginDirs       []string
	AllowedTools     []string
	PermissionMode   string // "" (manual), acceptEdits
	Cwd              string
	Env              map[string]string
}

// EventType names the normalized event kinds.
type EventType string

const (
	EvSystem     EventType = "system"
	EvText       EventType = "text"
	EvToolCall   EventType = "tool_call"
	EvToolResult EventType = "tool_result"
	EvPermission EventType = "permission"
	EvResult     EventType = "result"
	EvError      EventType = "error"
)

// Event is one normalized event from a session.
type Event struct {
	Type      EventType       `json:"type"`
	Seq       int             `json:"seq"`
	Time      time.Time       `json:"ts"`
	ToolName  string          `json:"tool_name,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	Text      string          `json:"text,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// Usage holds token counts from a result.
type Usage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
}

// Result is the terminal outcome of a session.
type Result struct {
	SessionID         string          `json:"session_id"`
	ModelResponded    string          `json:"model_responded"`
	Usage             Usage           `json:"usage"`
	VendorCostUSD     float64         `json:"vendor_cost_usd"`
	ResultText        string          `json:"result"`
	StructuredOutput  json.RawMessage `json:"structured_output,omitempty"`
	IsError           bool            `json:"is_error"`
	PermissionDenials int             `json:"permission_denials"`
	SkillVersions     []string        `json:"skill_versions,omitempty"`
	Raw               json.RawMessage `json:"-"`
}

// Session is one running agent process.
type Session interface {
	Events() <-chan Event
	Wait() (Result, error)
	Interrupt() error
}

// Driver starts sessions.
type Driver interface {
	Name() string
	Start(ctx context.Context, spec StepSpec) (Session, error)
}

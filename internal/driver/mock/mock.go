// Package mock is a scripted driver for tests and demos that need no model
// call. It emits a plausible event stream and returns structured output that
// matches the schema a step asks for, so the engine, store, trace graph and
// hooks can be exercised offline.
package mock

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gpn/gpn/internal/driver"
)

// Driver scripts sessions.
type Driver struct {
	// Outputs maps an output-schema basename (e.g. "tasks.json") to the
	// structured output to return. Missing keys fall back to built-ins.
	Outputs map[string]json.RawMessage
	// WriteFiles, when true, makes the Write tool event also write the file to
	// disk so downstream steps and lineage have something real to point at.
	WriteFiles bool
	// Model is reported as the responding model.
	Model string
}

// New returns a mock driver with defaults.
func New() *Driver {
	return &Driver{Outputs: map[string]json.RawMessage{}, WriteFiles: true, Model: "mock-model"}
}

// Name implements driver.Driver.
func (d *Driver) Name() string { return "mock" }

// Start implements driver.Driver.
func (d *Driver) Start(ctx context.Context, spec driver.StepSpec) (driver.Session, error) {
	s := &session{events: make(chan driver.Event, 16), done: make(chan struct{})}
	go d.script(spec, s)
	return s, nil
}

type session struct {
	events chan driver.Event
	done   chan struct{}
	result driver.Result
}

func (s *session) Events() <-chan driver.Event { return s.events }
func (s *session) Wait() (driver.Result, error) {
	<-s.done
	return s.result, nil
}
func (s *session) Interrupt() error { return nil }

func (d *Driver) script(spec driver.StepSpec, s *session) {
	now := time.Now()
	seq := 0
	emit := func(ev driver.Event) {
		seq++
		ev.Seq = seq
		ev.Time = now
		s.events <- ev
	}
	emit(driver.Event{Type: driver.EvSystem, Text: "init", Payload: json.RawMessage(`{"type":"system","subtype":"init","model":"` + d.Model + `","plugins":[{"name":"sdd-core","version":"0.1.0"}]}`)})
	emit(driver.Event{Type: driver.EvText, Text: "Working on " + spec.StepID})

	base := filepath.Base(spec.OutputSchemaPath)
	out, ok := d.Outputs[base]
	if !ok {
		out = builtin(base, spec)
	}
	// Simulate one file write so lineage has a CHANGED_IN edge.
	rel := "specs/" + spec.StepID + ".md"
	input, _ := json.Marshal(map[string]string{"file_path": rel, "content": "# " + spec.StepID + "\n\nmock output for run " + spec.RunID + "\n"})
	emit(driver.Event{Type: driver.EvToolCall, ToolName: "Write", ToolUseID: "toolu_mock_1", Input: input})
	if d.WriteFiles && spec.Cwd != "" {
		p := filepath.Join(spec.Cwd, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte("# "+spec.StepID+"\n\nmock output for run "+spec.RunID+"\n"), 0o644)
	}
	emit(driver.Event{Type: driver.EvToolResult, ToolUseID: "toolu_mock_1", Output: json.RawMessage(`"File written"`)})
	res := driver.Result{SessionID: "mock-" + spec.StepID, ModelResponded: d.Model, Usage: driver.Usage{InputTokens: 1200, OutputTokens: 300}, VendorCostUSD: 0.0042, ResultText: string(out), StructuredOutput: out, SkillVersions: []string{"sdd-core@0.1.0"}}
	emit(driver.Event{Type: driver.EvResult, Payload: res.Raw})
	s.result = res
	close(s.events)
	close(s.done)
}

func builtin(schema string, spec driver.StepSpec) json.RawMessage {
	switch {
	case strings.HasPrefix(schema, "requirements"):
		return json.RawMessage(`{"requirements":[{"id":"REQ-001","text":"WHEN a settlement file arrives THE SYSTEM SHALL validate its checksum","priority":"must"},{"id":"REQ-002","text":"WHEN validation fails THE SYSTEM SHALL quarantine the file and alert operations","priority":"must"}]}`)
	case strings.HasPrefix(schema, "design"):
		return json.RawMessage(`{"components":[{"name":"ingest","files":["internal/ingest/ingest.go"],"requirements":["REQ-001","REQ-002"]}],"contracts":[{"name":"SettlementFile","kind":"struct"}],"risks":["checksum algorithm mismatch with upstream"]}`)
	case strings.HasPrefix(schema, "tasks"):
		return json.RawMessage(`{"tasks":[{"id":"T1","title":"Add checksum validation","class":"implement","requirements":["REQ-001"],"files":["internal/ingest/ingest.go"],"depends_on":[]},{"id":"T2","title":"Quarantine and alert on failure","class":"implement","requirements":["REQ-002"],"files":["internal/ingest/quarantine.go"],"depends_on":["T1"]},{"id":"T3","title":"Update README","class":"mechanical","requirements":["REQ-001"],"files":["README.md"],"depends_on":[]}]}`)
	case strings.HasPrefix(schema, "verdict"):
		return json.RawMessage(`{"verdict":"pass","findings":[]}`)
	default:
		return json.RawMessage(fmt.Sprintf(`{"step":%q,"ok":true}`, spec.StepID))
	}
}

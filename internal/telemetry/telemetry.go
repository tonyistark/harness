// Package telemetry is a small span API with a JSONL file exporter. The span
// names and attribute keys follow the OpenTelemetry GenAI semantic
// conventions (invoke_workflow, invoke_agent, execute_tool, gen_ai.*) so the
// file can be replayed into an OTLP exporter later without renaming anything.
package telemetry

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Tracer starts spans.
type Tracer interface {
	Start(name string, attrs map[string]any) Span
	Close() error
}

// Span is a timed, attributed unit of work.
type Span interface {
	Set(key string, value any)
	End()
}

// NewFile returns a tracer that appends one JSON object per span to path.
func NewFile(path string) (Tracer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &fileTracer{f: f}, nil
}

// Nop returns a tracer that records nothing.
func Nop() Tracer { return nopTracer{} }

type fileTracer struct {
	mu sync.Mutex
	f  *os.File
}

func (t *fileTracer) Start(name string, attrs map[string]any) Span {
	s := &fileSpan{t: t, name: name, start: time.Now(), attrs: map[string]any{}}
	for k, v := range attrs {
		s.attrs[k] = v
	}
	return s
}

func (t *fileTracer) Close() error { return t.f.Close() }

type fileSpan struct {
	t     *fileTracer
	name  string
	start time.Time
	attrs map[string]any
	mu    sync.Mutex
}

func (s *fileSpan) Set(k string, v any) {
	s.mu.Lock()
	s.attrs[k] = v
	s.mu.Unlock()
}

func (s *fileSpan) End() {
	end := time.Now()
	s.mu.Lock()
	rec := map[string]any{"name": s.name, "start": s.start.UTC().Format(time.RFC3339Nano), "end": end.UTC().Format(time.RFC3339Nano), "duration_ms": end.Sub(s.start).Milliseconds(), "attributes": s.attrs}
	s.mu.Unlock()
	b, _ := json.Marshal(rec)
	s.t.mu.Lock()
	s.t.f.Write(append(b, '\n'))
	s.t.mu.Unlock()
}

type nopTracer struct{}

func (nopTracer) Start(string, map[string]any) Span { return nopSpan{} }
func (nopTracer) Close() error                      { return nil }

type nopSpan struct{}

func (nopSpan) Set(string, any) {}
func (nopSpan) End()            {}

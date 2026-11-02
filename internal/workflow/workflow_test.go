package workflow

import (
	"testing"

	"github.com/gpn/gpn/internal/assets"
)

func TestSeedFeatureWorkflowWaves(t *testing.T) {
	b, err := assets.Read("workflows/feature.yaml")
	if err != nil {
		t.Fatal(err)
	}
	wf, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	waves, err := wf.Waves()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"intake", "approve_requirements", "design", "tasks", "implement", "review", "gates", "merge", "pr"}
	if len(waves) != len(want) {
		t.Fatalf("got %d waves, want %d: %v", len(waves), len(want), waves)
	}
	for i, w := range want {
		if len(waves[i]) != 1 || waves[i][0] != w {
			t.Fatalf("wave %d = %v, want [%s]", i, waves[i], w)
		}
	}
}

func TestCycleDetected(t *testing.T) {
	_, err := Parse([]byte(`
name: cyc
steps:
  - { id: a, kind: shell, command: "true", after: [b] }
  - { id: b, kind: shell, command: "true", after: [a] }
`))
	if err == nil {
		t.Fatal("expected cycle error")
	}
}

func TestParallelWaves(t *testing.T) {
	wf, err := Parse([]byte(`
name: p
steps:
  - { id: a, kind: shell, command: "true" }
  - { id: b, kind: shell, command: "true", after: [a] }
  - { id: c, kind: shell, command: "true", after: [a] }
  - { id: d, kind: shell, command: "true", after: [b, c] }
`))
	if err != nil {
		t.Fatal(err)
	}
	waves, _ := wf.Waves()
	if len(waves) != 3 || len(waves[1]) != 2 {
		t.Fatalf("unexpected waves %v", waves)
	}
}

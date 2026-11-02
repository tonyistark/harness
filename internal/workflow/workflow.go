// Package workflow loads, validates and orders GPN workflow definitions.
//
// A workflow is a YAML file describing a DAG of steps. Four step kinds exist:
// agent (a Claude Code session), approval (a human decision), gate (a test or
// quality check) and shell (a command). parallel wraps a step over a list.
package workflow

import (
	"fmt"
	"os"
	"sort"

	"github.com/goccy/go-yaml"
)

// Input declares a workflow input.
type Input struct {
	Type     string `yaml:"type"`
	Required bool   `yaml:"required"`
}

// Defaults apply to every step unless the step overrides them.
type Defaults struct {
	Route     string  `yaml:"route"`
	BudgetUSD float64 `yaml:"budget_usd"`
}

// Step is one node of the DAG.
type Step struct {
	ID             string   `yaml:"id"`
	Kind           string   `yaml:"kind"`
	Class          string   `yaml:"class"`
	Skill          string   `yaml:"skill"`
	Prompt         string   `yaml:"prompt"`
	OutputSchema   string   `yaml:"output_schema"`
	After          []string `yaml:"after"`
	Approvers      []string `yaml:"approvers"`
	Timeout        string   `yaml:"timeout"`
	ForEach        string   `yaml:"for_each"`
	MaxConcurrency int      `yaml:"max_concurrency"`
	Isolation      string   `yaml:"isolation"`
	Step           *Step    `yaml:"step"`
	Checks         []string `yaml:"checks"`
	Command        string   `yaml:"command"`
	Tools          []string `yaml:"tools"`
	RetryOn        string   `yaml:"retry_on"`
	MaxLoops       int      `yaml:"max_loops"`
	BudgetUSD      float64  `yaml:"budget_usd"`
}

// Workflow is a parsed definition.
type Workflow struct {
	Name     string           `yaml:"name"`
	Version  int              `yaml:"version"`
	Inputs   map[string]Input `yaml:"inputs"`
	Defaults Defaults         `yaml:"defaults"`
	Steps    []Step           `yaml:"steps"`
}

var kinds = map[string]bool{"agent": true, "approval": true, "gate": true, "shell": true, "parallel": true}

// Load reads and validates a workflow file.
func Load(path string) (*Workflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes YAML and validates the result.
func Parse(data []byte) (*Workflow, error) {
	var w Workflow
	if err := yaml.Unmarshal(data, &w); err != nil {
		return nil, fmt.Errorf("workflow: parse: %w", err)
	}
	if err := w.Validate(); err != nil {
		return nil, err
	}
	return &w, nil
}

// Validate checks ids, kinds, references and acyclicity.
func (w *Workflow) Validate() error {
	if w.Name == "" {
		return fmt.Errorf("workflow: name is required")
	}
	if len(w.Steps) == 0 {
		return fmt.Errorf("workflow %q: no steps", w.Name)
	}
	seen := map[string]bool{}
	for i := range w.Steps {
		s := &w.Steps[i]
		if s.ID == "" {
			return fmt.Errorf("workflow %q: step %d has no id", w.Name, i)
		}
		if seen[s.ID] {
			return fmt.Errorf("workflow %q: duplicate step id %q", w.Name, s.ID)
		}
		seen[s.ID] = true
		if err := validateStep(s, true); err != nil {
			return fmt.Errorf("workflow %q: step %q: %w", w.Name, s.ID, err)
		}
	}
	for _, s := range w.Steps {
		for _, a := range s.After {
			if !seen[a] {
				return fmt.Errorf("workflow %q: step %q: after references unknown step %q", w.Name, s.ID, a)
			}
		}
	}
	if _, err := w.Waves(); err != nil {
		return err
	}
	return nil
}

func validateStep(s *Step, top bool) error {
	if !kinds[s.Kind] {
		return fmt.Errorf("unknown kind %q", s.Kind)
	}
	switch s.Kind {
	case "agent":
		if s.Class == "" {
			return fmt.Errorf("agent step needs a class (policy maps class to a model tier)")
		}
	case "approval":
		if len(s.Approvers) == 0 {
			return fmt.Errorf("approval step needs approvers")
		}
	case "gate":
		if len(s.Checks) == 0 {
			return fmt.Errorf("gate step needs checks")
		}
	case "shell":
		if s.Command == "" {
			return fmt.Errorf("shell step needs a command")
		}
	case "parallel":
		if !top {
			return fmt.Errorf("parallel steps cannot nest")
		}
		if s.ForEach == "" || s.Step == nil {
			return fmt.Errorf("parallel step needs for_each and step")
		}
		if s.Step.Kind == "approval" {
			return fmt.Errorf("parallel step cannot wrap an approval")
		}
		return validateStep(s.Step, false)
	}
	return nil
}

// Step returns the step with the given id, or nil.
func (w *Workflow) Step(id string) *Step {
	for i := range w.Steps {
		if w.Steps[i].ID == id {
			return &w.Steps[i]
		}
	}
	return nil
}

// Waves groups steps into execution waves: every step in a wave depends only
// on steps in earlier waves. Order within a wave follows the file order.
func (w *Workflow) Waves() ([][]string, error) {
	indeg := map[string]int{}
	order := map[string]int{}
	for i, s := range w.Steps {
		indeg[s.ID] = len(s.After)
		order[s.ID] = i
	}
	dependents := map[string][]string{}
	for _, s := range w.Steps {
		for _, a := range s.After {
			dependents[a] = append(dependents[a], s.ID)
		}
	}
	var waves [][]string
	placed := 0
	for placed < len(w.Steps) {
		var wave []string
		for _, s := range w.Steps {
			if indeg[s.ID] == 0 {
				wave = append(wave, s.ID)
			}
		}
		if len(wave) == 0 {
			return nil, fmt.Errorf("workflow %q: cycle detected among steps", w.Name)
		}
		sort.Slice(wave, func(i, j int) bool { return order[wave[i]] < order[wave[j]] })
		for _, id := range wave {
			indeg[id] = -1
			for _, d := range dependents[id] {
				indeg[d]--
			}
		}
		placed += len(wave)
		waves = append(waves, wave)
	}
	return waves, nil
}

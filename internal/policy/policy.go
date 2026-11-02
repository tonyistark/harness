// Package policy loads gpn-policy.yaml, compiles it to Claude Code settings,
// and decides whether a tool call may run. The same Decide function serves
// the PreToolUse hook, the PermissionRequest hook and the engine, so there is
// exactly one definition of what agents may do.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
)

// Policy is the parsed gpn-policy.yaml.
type Policy struct {
	Version           int                      `yaml:"version"`
	Autonomy          string                   `yaml:"autonomy"` // autonomous | supervised | manual
	ClaudeCodeVersion string                   `yaml:"claude_code_version"`
	Sandbox           SandboxCfg               `yaml:"sandbox"`
	ProtectedPaths    []string                 `yaml:"protected_paths"`
	Commands          CommandRules             `yaml:"commands"`
	Tools             ToolRules                `yaml:"tools"`
	ApprovalClasses   map[string]ApprovalClass `yaml:"approval_classes"`
	Models            ModelsCfg                `yaml:"models"`
	Budgets           Budgets                  `yaml:"budgets"`
	Registry          RegistryCfg              `yaml:"registry"`
	Gates             map[string]string        `yaml:"gates"`

	raw []byte
}

// SandboxCfg controls Claude Code's sandbox.
type SandboxCfg struct {
	Required     bool     `yaml:"required"`
	NetworkAllow []string `yaml:"network_allow"`
}

// CommandRules are glob patterns over shell commands.
type CommandRules struct {
	Deny    []string `yaml:"deny"`
	Approve []string `yaml:"approve"`
}

// ToolRules are Claude Code permission rules (Tool or Tool(pattern)).
type ToolRules struct {
	Allow   []string `yaml:"allow"`
	Approve []string `yaml:"approve"`
	Deny    []string `yaml:"deny"`
}

// ApprovalClass names a class of change that needs a human.
type ApprovalClass struct {
	Paths     []string `yaml:"paths"`
	Approvers []string `yaml:"approvers"`
	TwoPerson bool     `yaml:"two_person"`
	Timeout   string   `yaml:"timeout"`
}

// ModelsCfg maps task classes to tiers and tiers to catalog aliases.
type ModelsCfg struct {
	Catalog string            `yaml:"catalog"`
	Route   string            `yaml:"route"`
	Tiers   map[string]Tier   `yaml:"tiers"`
	Classes map[string]string `yaml:"classes"`
}

// Tier is one model tier.
type Tier struct {
	Catalog  string `yaml:"catalog"`
	Fallback string `yaml:"fallback"`
}

// Budgets cap spend.
type Budgets struct {
	PerStepUSD float64 `yaml:"per_step_usd"`
	PerRunUSD  float64 `yaml:"per_run_usd"`
	AlertAt    float64 `yaml:"alert_at"`
}

// RegistryCfg governs which skills may be installed.
type RegistryCfg struct {
	TiersAllowed     []string `yaml:"tiers_allowed"`
	RequireSignature bool     `yaml:"require_signature"`
}

// Catalog is the signed list of approved models.
type Catalog struct {
	Models    map[string]CatalogEntry `json:"models"`
	Signature string                  `json:"signature,omitempty"`
}

// CatalogEntry is one alias in the catalog.
type CatalogEntry struct {
	Route           map[string]string `json:"route"` // route name -> provider model id
	PriceInPerMTok  float64           `json:"price_in_per_mtok"`
	PriceOutPerMTok float64           `json:"price_out_per_mtok"`
	ClearedClasses  []string          `json:"cleared_classes"`
}

// Load reads and validates a policy file.
func Load(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes and validates policy YAML.
func Parse(data []byte) (*Policy, error) {
	var p Policy
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("policy: parse: %w", err)
	}
	p.raw = data
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Validate checks the policy for internal consistency.
func (p *Policy) Validate() error {
	switch p.Autonomy {
	case "", "autonomous", "supervised", "manual":
	default:
		return fmt.Errorf("policy: autonomy must be autonomous, supervised or manual")
	}
	if p.Autonomy == "" {
		p.Autonomy = "supervised"
	}
	if p.Models.Route == "" {
		return fmt.Errorf("policy: models.route is required")
	}
	for class, tier := range p.Models.Classes {
		if _, ok := p.Models.Tiers[tier]; !ok {
			return fmt.Errorf("policy: class %q maps to unknown tier %q", class, tier)
		}
	}
	for name, t := range p.Models.Tiers {
		if t.Catalog == "" {
			return fmt.Errorf("policy: tier %q has no catalog alias", name)
		}
		if t.Fallback != "" {
			if _, ok := p.Models.Tiers[t.Fallback]; !ok {
				return fmt.Errorf("policy: tier %q falls back to unknown tier %q", name, t.Fallback)
			}
		}
	}
	if p.Budgets.AlertAt == 0 {
		p.Budgets.AlertAt = 0.8
	}
	return nil
}

// CheckRuntimeVersion compares an installed Claude Code version ("2.1.316")
// with the policy's claude_code_version constraint (">=2.1.300", "2.1.300",
// or empty for no constraint).
func (p *Policy) CheckRuntimeVersion(installed string) error {
	want := strings.TrimSpace(p.ClaudeCodeVersion)
	if want == "" {
		return nil
	}
	op := ">="
	for _, o := range []string{">=", "==", "="} {
		if strings.HasPrefix(want, o) {
			op = o
			want = strings.TrimSpace(want[len(o):])
			break
		}
	}
	have, err := parseSemver(installed)
	if err != nil {
		return fmt.Errorf("policy: cannot parse installed Claude Code version %q", installed)
	}
	min, err := parseSemver(want)
	if err != nil {
		return fmt.Errorf("policy: cannot parse claude_code_version %q", p.ClaudeCodeVersion)
	}
	cmp := compareSemver(have, min)
	switch op {
	case ">=":
		if cmp < 0 {
			return fmt.Errorf("Claude Code %s is installed but gpn-policy.yaml requires claude_code_version %s. Upgrade (npm i -g @anthropic-ai/claude-code@latest) or lower the floor in the policy", installed, p.ClaudeCodeVersion)
		}
	default:
		if cmp != 0 {
			return fmt.Errorf("Claude Code %s is installed but gpn-policy.yaml pins claude_code_version %s", installed, p.ClaudeCodeVersion)
		}
	}
	return nil
}

func parseSemver(v string) ([3]int, error) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+ "); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return out, fmt.Errorf("not a version: %q", v)
	}
	for i, part := range parts {
		n := 0
		if _, err := fmt.Sscanf(part, "%d", &n); err != nil {
			return out, fmt.Errorf("not a version: %q", v)
		}
		out[i] = n
	}
	return out, nil
}

func compareSemver(a, b [3]int) int {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Hash is the SHA-256 of the policy file, recorded on every run.
func (p *Policy) Hash() string {
	sum := sha256.Sum256(p.raw)
	return hex.EncodeToString(sum[:])
}

// LoadCatalog reads the model catalog the policy points at (path relative to
// the policy file's directory when not absolute).
func (p *Policy) LoadCatalog(policyDir string) (*Catalog, error) {
	path := p.Models.Catalog
	if path == "" {
		return nil, fmt.Errorf("policy: models.catalog is required")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(policyDir, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Catalog
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	return &c, nil
}

// Resolution is the model choice for a step.
type Resolution struct {
	Class    string
	Tier     string
	Route    string
	Model    string
	Fallback string
	Entry    CatalogEntry
}

// ResolveModel maps a task class to a concrete model id on a route.
func (p *Policy) ResolveModel(class string, cat *Catalog, routeOverride string) (Resolution, error) {
	tierName, ok := p.Models.Classes[class]
	if !ok {
		return Resolution{}, fmt.Errorf("policy: no tier for class %q", class)
	}
	tier := p.Models.Tiers[tierName]
	route := p.Models.Route
	if routeOverride != "" {
		route = routeOverride
	}
	entry, ok := cat.Models[tier.Catalog]
	if !ok {
		return Resolution{}, fmt.Errorf("catalog: alias %q not in signed catalog", tier.Catalog)
	}
	model, ok := entry.Route[route]
	if !ok {
		return Resolution{}, fmt.Errorf("catalog: alias %q has no model on route %q", tier.Catalog, route)
	}
	if !contains(entry.ClearedClasses, class) && !contains(entry.ClearedClasses, "*") {
		return Resolution{}, fmt.Errorf("catalog: alias %q is not cleared for class %q", tier.Catalog, class)
	}
	res := Resolution{Class: class, Tier: tierName, Route: route, Model: model, Entry: entry}
	if tier.Fallback != "" {
		if fe, ok := cat.Models[p.Models.Tiers[tier.Fallback].Catalog]; ok {
			res.Fallback = fe.Route[route]
		}
	}
	return res, nil
}

// Decision is the outcome of Decide.
type Decision struct {
	Behavior string // allow | deny | ask
	Reason   string
	Class    string // approval class when Behavior == ask
}

// Decide evaluates one tool call against the policy. Deny wins over ask, ask
// wins over allow, and anything unmatched falls through to Claude Code's own
// permission flow as "allow" from the policy's point of view (the compiled
// settings still apply their rules).
func (p *Policy) Decide(toolName string, input map[string]any) Decision {
	cmd, _ := input["command"].(string)
	path := pathOf(input)

	for _, pat := range p.Commands.Deny {
		if toolName == "Bash" && matchGlob(pat, cmd) {
			return Decision{Behavior: "deny", Reason: "commands.deny: " + pat}
		}
	}
	for _, pp := range p.ProtectedPaths {
		if path != "" && matchPath(pp, path) && isWriteTool(toolName) {
			return Decision{Behavior: "deny", Reason: "protected_paths: " + pp}
		}
		if toolName == "Bash" && cmd != "" && mentionsProtected(pp, cmd) {
			return Decision{Behavior: "deny", Reason: "protected_paths (command mentions): " + pp}
		}
	}
	for _, rule := range p.Tools.Deny {
		if matchRule(rule, toolName, cmd, path) {
			return Decision{Behavior: "deny", Reason: "tools.deny: " + rule}
		}
	}
	for _, pat := range p.Commands.Approve {
		if toolName == "Bash" && matchGlob(pat, cmd) {
			return Decision{Behavior: "ask", Reason: "commands.approve: " + pat, Class: "command"}
		}
	}
	for _, rule := range p.Tools.Approve {
		if matchRule(rule, toolName, cmd, path) {
			return Decision{Behavior: "ask", Reason: "tools.approve: " + rule, Class: "tool"}
		}
	}
	if path != "" && isWriteTool(toolName) {
		names := make([]string, 0, len(p.ApprovalClasses))
		for n := range p.ApprovalClasses {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			for _, pp := range p.ApprovalClasses[n].Paths {
				if matchPath(pp, path) {
					return Decision{Behavior: "ask", Reason: "approval_classes." + n + ": " + pp, Class: n}
				}
			}
		}
	}
	return Decision{Behavior: "allow"}
}

func pathOf(input map[string]any) string {
	for _, k := range []string{"file_path", "path", "notebook_path"} {
		if s, ok := input[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func isWriteTool(name string) bool {
	switch name {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return true
	}
	return false
}

// matchRule matches Claude Code's permission rule syntax: "Tool" or
// "Tool(pattern)" where the pattern is a prefix glob for Bash commands or a
// path glob for file tools, and "mcp__server__tool" with trailing * allowed.
func matchRule(rule, toolName, cmd, path string) bool {
	rule = strings.TrimSpace(rule)
	if i := strings.Index(rule, "("); i > 0 && strings.HasSuffix(rule, ")") {
		tool := rule[:i]
		pat := rule[i+1 : len(rule)-1]
		if tool != toolName {
			return false
		}
		if toolName == "Bash" {
			return matchGlob(pat, cmd)
		}
		return path != "" && matchPath(pat, path)
	}
	return matchGlob(rule, toolName)
}

// matchGlob matches with '*' as a wildcard anywhere; other characters literal.
func matchGlob(pattern, s string) bool {
	if pattern == "" {
		return false
	}
	re := globToRegexp(pattern, false)
	return re.MatchString(s)
}

// matchPath matches a path glob where '**' crosses directories, '*' does not,
// and a leading '~/' expands to the home directory.
func matchPath(pattern, path string) bool {
	if strings.HasPrefix(pattern, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			pattern = filepath.Join(home, pattern[2:])
		}
	}
	path = filepath.Clean(path)
	re := globToRegexp(pattern, true)
	if re.MatchString(path) {
		return true
	}
	// A relative pattern matches any path that ends with it (worktrees sit
	// under .gpn/wt/<task>/ so the agent sees repository-relative paths).
	if !filepath.IsAbs(pattern) {
		return regexp.MustCompile("(^|/)" + re.String()[1:]).MatchString(path)
	}
	return false
}

func globToRegexp(pattern string, pathMode bool) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch {
		case c == '*' && pathMode && i+1 < len(pattern) && pattern[i+1] == '*':
			b.WriteString(".*")
			i++
			if i+1 < len(pattern) && pattern[i+1] == '/' {
				i++
				b.WriteString("/?")
			}
		case c == '*':
			if pathMode {
				b.WriteString("[^/]*")
			} else {
				b.WriteString(".*")
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// mentionsProtected is a coarse check that a shell command names a protected
// path (e.g. `echo x > .git/hooks/pre-commit`).
func mentionsProtected(pattern, cmd string) bool {
	base := strings.TrimSuffix(strings.TrimSuffix(pattern, "/**"), "/*")
	base = strings.TrimPrefix(base, "~/")
	if base == "" || base == "." {
		return false
	}
	return strings.Contains(cmd, base)
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

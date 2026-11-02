// Package gates runs the Layer 1 and Layer 2 checks a workflow names (build,
// test, coverage>=80, sast, sca) as shell commands configured in the policy's
// gates section, and turns each into a pass/fail TestRun.
package gates

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Result is one gate outcome.
type Result struct {
	Check    string        `json:"check"`
	Name     string        `json:"name"`
	Command  string        `json:"command"`
	Status   string        `json:"status"` // pass | fail | skipped
	Value    float64       `json:"value,omitempty"`
	Detail   string        `json:"detail,omitempty"`
	Duration time.Duration `json:"duration_ns"`
}

// Run executes one check. For threshold checks such as coverage>=80 the
// command's output is scanned for the last percentage, which is compared.
func Run(ctx context.Context, check, name, command, cwd string) Result {
	start := time.Now()
	res := Result{Check: check, Name: name, Command: command}
	if command == "" {
		res.Status = "skipped"
		res.Detail = "no command configured"
		return res
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = cwd
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	res.Duration = time.Since(start)
	res.Detail = tail(out.String(), 2000)
	op, threshold, hasThreshold := parseThreshold(check)
	if hasThreshold {
		v, ok := lastPercent(out.String())
		if !ok {
			res.Status = "fail"
			res.Detail = "no percentage found in output; " + res.Detail
			return res
		}
		res.Value = v
		if compare(v, op, threshold) && err == nil {
			res.Status = "pass"
		} else {
			res.Status = "fail"
		}
		return res
	}
	if err != nil {
		res.Status = "fail"
		return res
	}
	res.Status = "pass"
	return res
}

func parseThreshold(check string) (op string, value float64, ok bool) {
	for _, o := range []string{">=", "<=", ">", "<", "=="} {
		if i := strings.Index(check, o); i > 0 {
			v, err := strconv.ParseFloat(strings.TrimSpace(check[i+len(o):]), 64)
			if err != nil {
				return "", 0, false
			}
			return o, v, true
		}
	}
	return "", 0, false
}

var pct = regexp.MustCompile(`(\d+(?:\.\d+)?)%`)

func lastPercent(s string) (float64, bool) {
	m := pct.FindAllStringSubmatch(s, -1)
	if len(m) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[len(m)-1][1], 64)
	return v, err == nil
}

func compare(v float64, op string, t float64) bool {
	switch op {
	case ">=":
		return v >= t
	case "<=":
		return v <= t
	case ">":
		return v > t
	case "<":
		return v < t
	case "==":
		return v == t
	}
	return false
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// String renders a result for the console.
func (r Result) String() string {
	if r.Value != 0 {
		return fmt.Sprintf("%-14s %-4s %.1f%% (%s)", r.Check, r.Status, r.Value, r.Duration.Round(time.Millisecond))
	}
	return fmt.Sprintf("%-14s %-4s (%s)", r.Check, r.Status, r.Duration.Round(time.Millisecond))
}

// Package hook implements the Claude Code hook handlers behind `gpn hook`.
//
// Claude Code calls the gpn binary with JSON on stdin for PreToolUse,
// PermissionRequest, PostToolUse and Stop. The handlers apply the policy
// (deny by exit code 2 or a decision object), record events and lineage, and
// hold PermissionRequest decisions for a human when the policy says ask.
package hook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gpn/gpn/internal/policy"
	"github.com/gpn/gpn/internal/store"
	"github.com/gpn/gpn/internal/trace"
)

// Input is the common hook stdin shape plus the tool fields.
type Input struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	Cwd            string          `json:"cwd"`
	PermissionMode string          `json:"permission_mode"`
	HookEventName  string          `json:"hook_event_name"`
	ToolName       string          `json:"tool_name"`
	ToolInput      map[string]any  `json:"tool_input"`
	ToolUseID      string          `json:"tool_use_id"`
	ToolResponse   json.RawMessage `json:"tool_response"`
}

// Env carries the run context the driver puts in the environment.
type Env struct {
	RunID  string
	StepID string
}

// EnvFromProcess reads GPN_RUN_ID and GPN_STEP_ID.
func EnvFromProcess() Env {
	return Env{RunID: os.Getenv("GPN_RUN_ID"), StepID: os.Getenv("GPN_STEP_ID")}
}

func readInput(r io.Reader) (Input, error) {
	var in Input
	b, err := io.ReadAll(r)
	if err != nil {
		return in, err
	}
	if err := json.Unmarshal(b, &in); err != nil {
		return in, fmt.Errorf("hook input: %w", err)
	}
	return in, nil
}

// Pre handles PreToolUse: deny by exit code 2 (stderr carries the reason),
// ask by permissionDecision "ask", otherwise stay silent and let Claude Code's
// own permission flow continue. Returns the process exit code.
func Pre(stdin io.Reader, stdout, stderr io.Writer, p *policy.Policy, st *store.Store, env Env) int {
	in, err := readInput(stdin)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2 // malformed input never lets a call through
	}
	d := p.Decide(in.ToolName, in.ToolInput)
	if st != nil {
		recordDecision(st, env, in, "pre", d)
	}
	switch d.Behavior {
	case "deny":
		fmt.Fprintf(stderr, "gpn policy: %s", d.Reason)
		return 2
	case "ask":
		out := map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse", "permissionDecision": "ask", "permissionDecisionReason": "gpn policy: " + d.Reason}}
		json.NewEncoder(stdout).Encode(out)
		return 0
	}
	return 0
}

// Permission handles PermissionRequest. Exit code 2 is not honored on this
// event, so the decision object is the only signal: allow or deny from policy,
// or wait for a human decision recorded with `gpn approve`.
func Permission(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, p *policy.Policy, st *store.Store, env Env, wait time.Duration, poll time.Duration) int {
	in, err := readInput(stdin)
	if err != nil {
		writeDecision(stdout, "deny", "gpn: malformed hook input", nil)
		return 0
	}
	d := p.Decide(in.ToolName, in.ToolInput)
	if st != nil {
		recordDecision(st, env, in, "permission", d)
	}
	switch d.Behavior {
	case "deny":
		writeDecision(stdout, "deny", "gpn policy: "+d.Reason, nil)
		return 0
	case "allow":
		if p.Autonomy == "manual" {
			// Manual autonomy: a human answers every prompt.
			break
		}
		writeDecision(stdout, "allow", "", in.ToolInput)
		return 0
	}
	if st == nil {
		writeDecision(stdout, "deny", "gpn: no store available to hold the approval", nil)
		return 0
	}
	call, _ := json.Marshal(map[string]any{"tool_name": in.ToolName, "tool_input": in.ToolInput, "reason": d.Reason})
	class := d.Class
	if class == "" {
		class = "manual"
	}
	a, err := st.RequestApproval(store.Approval{RunID: env.RunID, StepID: env.StepID, Class: class, ToolCallJSON: string(call)})
	if err != nil {
		writeDecision(stdout, "deny", "gpn: could not record approval: "+err.Error(), nil)
		return 0
	}
	fmt.Fprintf(stderr, "gpn: approval #%d pending (%s) — gpn approve %s %s --id %d\n", a.ID, d.Reason, env.RunID, env.StepID, a.ID)
	deadline := time.Now().Add(wait)
	if poll <= 0 {
		poll = 2 * time.Second
	}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			writeDecision(stdout, "deny", "gpn: cancelled while waiting for approval", nil)
			return 0
		case <-time.After(poll):
		}
		cur, err := st.GetApproval(a.ID)
		if err != nil {
			continue
		}
		switch cur.Decision {
		case "approve":
			writeDecision(stdout, "allow", "", in.ToolInput)
			return 0
		case "deny":
			writeDecision(stdout, "deny", "denied by "+cur.Decider+": "+cur.Reason, nil)
			return 0
		}
	}
	writeDecision(stdout, "deny", fmt.Sprintf("gpn: approval #%d not decided within %s; resume the run after deciding", a.ID, wait), nil)
	return 0
}

func writeDecision(w io.Writer, behavior, message string, updatedInput map[string]any) {
	decision := map[string]any{"behavior": behavior}
	if message != "" {
		decision["message"] = message
	}
	if behavior == "allow" && updatedInput != nil {
		decision["updatedInput"] = updatedInput
	}
	json.NewEncoder(w).Encode(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PermissionRequest", "decision": decision}})
}

// Post handles PostToolUse: record the call and, for file tools, a
// File node with a CHANGED_IN edge to the agent run. Never blocks.
func Post(stdin io.Reader, stdout, stderr io.Writer, st *store.Store, env Env) int {
	in, err := readInput(stdin)
	if err != nil || st == nil {
		return 0
	}
	stepRow, ok, err := st.LatestStep(env.RunID, env.StepID)
	if err != nil || !ok {
		return 0
	}
	n, _ := st.CountEvents(stepRow.ID)
	inB, _ := json.Marshal(in.ToolInput)
	_ = st.InsertEvent(store.Event{StepRowID: stepRow.ID, Seq: 100000 + n, Type: "hook_post", ToolName: in.ToolName, ToolUseID: in.ToolUseID, InputHash: hash(inB), OutputHash: hash(in.ToolResponse), Payload: string(inB), Time: time.Now()})
	if path := pathOf(in.ToolInput); path != "" && isWrite(in.ToolName) {
		rel := path
		if in.Cwd != "" && filepath.IsAbs(path) {
			if r, err := filepath.Rel(in.Cwd, path); err == nil && !strings.HasPrefix(r, "..") {
				rel = r
			}
		}
		g := &trace.Graph{S: st}
		fid, _ := g.Put(trace.File, rel, nil)
		runNode := trace.ID(trace.AgentRun, env.RunID+"/"+env.StepID)
		_ = g.Link(fid, runNode, trace.ChangedIn, map[string]any{"tool": in.ToolName})
	}
	return 0
}

// Stop handles Stop: close the step's hook trail. Never blocks.
func Stop(stdin io.Reader, stdout, stderr io.Writer, st *store.Store, env Env) int {
	in, err := readInput(stdin)
	if err != nil || st == nil {
		return 0
	}
	if stepRow, ok, err := st.LatestStep(env.RunID, env.StepID); err == nil && ok {
		n, _ := st.CountEvents(stepRow.ID)
		_ = st.InsertEvent(store.Event{StepRowID: stepRow.ID, Seq: 100000 + n, Type: "hook_stop", Payload: `{"session_id":"` + in.SessionID + `"}`, Time: time.Now()})
	}
	return 0
}

func recordDecision(st *store.Store, env Env, in Input, phase string, d policy.Decision) {
	stepRow, ok, err := st.LatestStep(env.RunID, env.StepID)
	if err != nil || !ok {
		return
	}
	n, _ := st.CountEvents(stepRow.ID)
	payload, _ := json.Marshal(map[string]any{"phase": phase, "tool": in.ToolName, "decision": d.Behavior, "reason": d.Reason, "class": d.Class})
	inB, _ := json.Marshal(in.ToolInput)
	_ = st.InsertEvent(store.Event{StepRowID: stepRow.ID, Seq: 100000 + n, Type: "policy_decision", ToolName: in.ToolName, ToolUseID: in.ToolUseID, InputHash: hash(inB), Payload: string(payload), Time: time.Now()})
}

func pathOf(input map[string]any) string {
	for _, k := range []string{"file_path", "path", "notebook_path"} {
		if s, ok := input[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func isWrite(tool string) bool {
	switch tool {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return true
	}
	return false
}

func hash(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

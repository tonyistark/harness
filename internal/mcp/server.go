// Package mcp is a minimal Model Context Protocol server over stdio that
// exposes two tools to Claude Code sessions: trace_query (read the
// traceability graph) and approve (record a human decision from an IDE
// session). It speaks newline-delimited JSON-RPC 2.0.
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/gpn/gpn/internal/store"
	"github.com/gpn/gpn/internal/trace"
)

// Server serves one stdio connection.
type Server struct {
	Store           *store.Store
	Repo            string
	ProtocolVersion string
	Name            string
	Version         string
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve reads requests from r and writes responses to w until EOF.
func (s *Server) Serve(r io.Reader, w io.Writer) error {
	if s.ProtocolVersion == "" {
		s.ProtocolVersion = "2025-06-18"
	}
	if s.Name == "" {
		s.Name = "gpn"
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	enc := json.NewEncoder(w)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			enc.Encode(response{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		if req.ID == nil { // notification
			continue
		}
		res := s.handle(req)
		enc.Encode(res)
	}
	return sc.Err()
}

func (s *Server) handle(req request) response {
	resp := response{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": s.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": s.tools()}
	case "tools/call":
		var p struct {
			Name string         `json:"name"`
			Args map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: "invalid params"}
			return resp
		}
		out, err := s.call(p.Name, p.Args)
		if err != nil {
			resp.Result = map[string]any{"content": []any{map[string]any{"type": "text", "text": err.Error()}}, "isError": true}
			return resp
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		resp.Result = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}}
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

func (s *Server) tools() []any {
	return []any{
		map[string]any{
			"name":        "trace_query",
			"description": "Query the GPN traceability graph. name is one of coverage_gap, orphans, impact (node), blast_radius (file), why (file, line).",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"name": map[string]any{"type": "string"},
				"node": map[string]any{"type": "string"},
				"file": map[string]any{"type": "string"},
				"line": map[string]any{"type": "integer"},
			}, "required": []string{"name"}},
			"annotations": map[string]any{"readOnlyHint": true, "destructiveHint": false, "openWorldHint": false},
		},
		map[string]any{
			"name":        "approve",
			"description": "Record a human decision on a pending GPN approval. Only a person should call this.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"approval_id": map[string]any{"type": "integer"},
				"decision":    map[string]any{"type": "string", "enum": []string{"approve", "deny"}},
				"decider":     map[string]any{"type": "string"},
				"reason":      map[string]any{"type": "string"},
			}, "required": []string{"approval_id", "decision", "decider"}},
			"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
		},
	}
}

func (s *Server) call(name string, args map[string]any) (any, error) {
	switch name {
	case "trace_query":
		g := &trace.Graph{S: s.Store}
		q, _ := args["name"].(string)
		sargs := map[string]string{"repo": s.Repo}
		for _, k := range []string{"node", "file"} {
			if v, ok := args[k].(string); ok {
				sargs[k] = v
			}
		}
		if v, ok := args["line"].(float64); ok {
			sargs["line"] = strconv.Itoa(int(v))
		}
		rows, err := g.Query(q, sargs)
		if rows == nil && err == nil {
			rows = []trace.Row{}
		}
		return rows, err
	case "approve":
		idf, _ := args["approval_id"].(float64)
		decision, _ := args["decision"].(string)
		decider, _ := args["decider"].(string)
		reason, _ := args["reason"].(string)
		if err := s.Store.DecideApproval(int64(idf), decider, decision, reason); err != nil {
			return nil, err
		}
		return map[string]any{"approval_id": int64(idf), "decision": decision}, nil
	}
	return nil, fmt.Errorf("unknown tool %q", name)
}

// ServeStdio runs the server on the process's stdin/stdout.
func (s *Server) ServeStdio() error { return s.Serve(os.Stdin, os.Stdout) }

# gpn — GPN Agentic Harness (prototype)

One Go binary that drives Claude Code headless through governed, journaled,
traceable spec-driven workflows on a single machine. It adds the four things
Claude Code does not provide on its own:

| Capability | What `gpn` does | Where |
| --- | --- | --- |
| Workflow engine | YAML DAGs of `agent`, `approval`, `gate`, `shell` and `parallel` steps; every step journaled in SQLite; crash → `gpn resume` continues without re-running finished steps; approvals survive restarts | `internal/workflow`, `internal/engine` |
| Policy | One `gpn-policy.yaml` compiled to Claude Code settings (`--settings`, fail-closed sandbox, permission rules) **and** enforced by `gpn hook` from inside Claude Code's PreToolUse / PermissionRequest / PostToolUse / Stop events, so a Bedrock session that server-managed settings cannot reach is still governed | `internal/policy`, `internal/hook` |
| Skill registry | Claude Code plugin layout + `gpn.manifest.yaml`; canonical digest; ed25519 signature (cosign/Sigstore at GA); tier check and verified install; `gpn eval` wraps `claude plugin eval` as a publish gate | `internal/registry`, `internal/evals` |
| Traceability and audit | Requirement ↔ task ↔ file ↔ commit ↔ agent run ↔ model ↔ skill graph in SQLite with coverage-gap, impact, blast-radius, orphan and "why did this line change" queries; hash-chained audit log; per-step cost | `internal/trace`, `internal/audit`, `internal/cost` |

Design: see the Claude Doc *GPN Agentic Harness — Prototype Architecture (Go,
single machine)*; requirements: *GPN Agentic Harness — Requirements*.

## Status

Scaffold, end of week 0. Builds and passes its tests; the end-to-end feature
workflow (intake → approval → design → tasks → parallel implementation in
worktrees → review → gates → merge → PR) runs against the **mock driver**. The
Claude Code driver (`claude -p --bare --output-format stream-json`) is
implemented and unit-tested against recorded event shapes but has not yet run
against a live, credentialed Claude Code on a real repository — that is the
first task of week 1.

Tested on a Mac with an older Claude Code on Nov 1: `make install`, `gpn init`, the
mock run, `--dry-run`, `gpn audit verify` and `make test` all worked; the live run
failed on `unknown option '--bare'`, which is why the driver now probes the
installed version and flags and the policy carries a version floor.

Known gaps, in priority order: live Claude Code run; structured-output schema
validation (`--json-schema` is passed, the engine does not yet validate the
reply itself); the `review` loop on `changes_requested`; `gpn init` pre-commit
hook for trailers; OTLP exporter (file exporter only); Jira/GitHub connectors
(the MCP config carries only the `gpn` server today).

## Install

Prerequisites on the machine: macOS or Linux (Windows via WSL2 — Claude Code's
sandbox does not run natively on Windows), git 2.40+, Claude Code 2.1.300+
authenticated for Bedrock (`CLAUDE_CODE_USE_BEDROCK=1` and AWS credentials) or
the Anthropic API (`ANTHROPIC_API_KEY`; bare mode does not use the subscription
login), and Go 1.24 only if you build from source.

```bash
make install              # or: go install ./cmd/gpn   (CGO_ENABLED=0, pure Go SQLite)
gpn version               # prints the gpn version and the Claude Code version and flags it detected

cd /path/to/the/pilot/repo
gpn init                  # policy, constitution, workflows, schemas, prompts, seed registry (.gpn/registry/sdd-core), signing key
open .gpn/model-catalog.json      # put your approved model ids in; the seed has placeholders (any editor)
open gpn-policy.yaml              # gates: commands for this repo; network allowlist; tiers; claude_code_version floor
gpn policy compile
```

Claude Code version: the seed policy requires `claude_code_version: ">=2.1.300"`
because `--bare`, `--fallback-model`, `--max-budget-usd` and `--json-schema`
arrived in the 2.1.x line. `gpn run` refuses with a clear message when the
installed version is below the floor; upgrade with
`npm i -g @anthropic-ai/claude-code@latest`, or lower the floor in the policy.
Below the floor the driver still omits any flag the installed Claude Code lacks
(with a warning) rather than failing on `unknown option`. If your version
rejects `--json-schema` together with `stream-json`, set `GPN_NO_JSON_SCHEMA=1`;
the engine still recovers JSON from the result text.

`go.mod` carries two `replace` lines pointing `golang.org/x/*` at their GitHub
mirrors because the environment this was scaffolded in could not reach
golang.org. They are harmless and can be removed on a machine with normal
module access.

## Run

```bash
gpn run feature --input jira=GPN-123                 # Demo 1: spec to PR; prints the run id and pauses at the approval
gpn status                                           # list runs
gpn status <run-id>                                  # steps, models, cost, pending approvals
gpn approve <run-id> approve_requirements --by tony --reason "scope confirmed"
gpn resume <run-id>                                  # continues; finished steps are not re-run
gpn stop <run-id>                                    # halts at the next safe point

gpn run feature --input jira=GPN-123 --route anthropic --set classes.implement=haiku   # Demo 2

# Demo 3 — each query is its own command (file must be committed in this repo):
gpn trace why <path/to/file.go>:<line>               # line → commit → task → requirement → run → model, skills, approver, cost
gpn trace coverage                                   # requirements with no passing verified test
gpn trace orphans                                    # agent-changed files with no task
gpn trace impact Requirement:REQ-001                 # downstream of a requirement, marked SUSPECT
gpn trace blast <path/to/file.go>                    # requirements served by a file
gpn audit verify

gpn run feature --input jira=GPN-123 --driver mock   # the whole loop without a model call
gpn run feature --input jira=GPN-123 --dry-run       # resolve models and journal, no execution
```

Mock run end to end (what the tests do, by hand):

```bash
gpn run feature --input jira=GPN-123 --driver mock     # pauses; note the run id it prints
gpn approve <run-id> approve_requirements --by tony
gpn resume <run-id> --driver mock
gpn trace why "specs/implement[T1].md:1"               # the mock writes this file in task T1's worktree
```

Exit codes: 0 done · 1 error · 3 waiting for approval · 4 blocked (gate or
budget) · 5 stopped.

## Registry and evals

`gpn init` already generates the `local` key pair, publishes the seed plugin
with it and installs it. The commands below are for your own plugins and for
re-verifying.

```bash
gpn registry list                                    # installed plugins, tier, digest, path
gpn registry verify                                  # re-verify every installed plugin against its signature
gpn registry publish .gpn/registry/sdd-core          # re-sign after editing a skill (digest + signature into the plugin dir)
gpn registry install .gpn/registry/sdd-core          # verify signature and digest, check tier, record
gpn eval .gpn/registry/sdd-core                      # claude plugin eval: pinned judge, 3 runs, threshold, cost cap; report archived
gpn registry keygen <id>                             # a NEW key id; keygen refuses to overwrite an existing key
```

Keys live in `~/.gpn/keys/` (`GPN_KEYS_DIR` overrides). Rotating the `local`
key invalidates every signature it made: `gpn registry verify` then reports
"the key was regenerated", and `gpn registry publish` + `install` on each
plugin fixes it.

Eval cases live under `<plugin>/evals/**/case.yaml`; `claude plugin eval init`
scaffolds the authoritative format if the seed cases drift from it.

## How a step runs

```
gpn run ─▶ engine (journal step) ─▶ policy.ResolveModel(class) ─▶ driver
   claude -p "<prompt>" --bare --output-format stream-json --verbose
          --settings .gpn/claude-settings.json --mcp-config .gpn/mcp.json
          --model <tier model> --fallback-model <next tier> --max-budget-usd <step budget>
          --json-schema <schema> --append-system-prompt-file gpn-constitution.md
          --plugin-dir .gpn/registry/sdd-core
   Claude Code ─▶ PreToolUse ─▶ gpn hook pre      (deny list, protected paths; exit 2 blocks)
               ─▶ PermissionRequest ─▶ gpn hook permission (policy answer, or waits for `gpn approve`)
               ─▶ PostToolUse ─▶ gpn hook post    (events, CHANGED_IN lineage)
               ─▶ MCP gpn ─▶ trace_query / approve
   result event ─▶ cost, model responded, skill versions, structured output ─▶ journal, graph, audit
```

Hooks and the MCP server find the policy and database through the `--policy`
and `--db` arguments the compiler writes into the settings, and the run and
step through `GPN_RUN_ID` / `GPN_STEP_ID` in the Claude Code process
environment.

## Layout

```
cmd/gpn/                 CLI (stdlib flag; flags may appear anywhere on the line)
internal/workflow        YAML schema, validation, DAG waves
internal/engine          journaled executor, approvals, parallel worktrees, gates, merge, lineage
internal/driver          Driver/Session/Event interfaces
internal/driver/claudecode   claude -p stream-json driver
internal/driver/mock         scripted driver for tests and demos
internal/policy          policy model, Decide(), compile to Claude Code settings + MCP config
internal/hook            PreToolUse / PermissionRequest / PostToolUse / Stop handlers
internal/mcp             stdio MCP server: trace_query, approve
internal/registry        manifests, digest, ed25519 sign/verify, tiered install
internal/evals           claude plugin eval wrapper + report archive
internal/trace           graph types, queries, why
internal/audit           hash chain
internal/cost            tokens → USD from the catalog
internal/telemetry       span API, JSONL exporter (GenAI semconv names)
internal/store           SQLite (pure Go), migrations, repositories
internal/gates           gate runners with thresholds
internal/vcs             git worktrees, trailers, merge, blame
internal/assets/seed     files `gpn init` writes: policy, catalog, workflows, schemas, prompts, constitution, sdd-core plugin
```

## Tests

```bash
make test
```

`internal/engine` runs the full feature workflow on the mock driver in a
temporary git repository: approval pause and resume, parallel worktrees,
tier routing (mechanical → haiku), gates, merge with trailers, `why`,
coverage gap, blast radius, audit verification, route and tier overrides,
gate failure blocking the merge, and a denied approval failing the run.

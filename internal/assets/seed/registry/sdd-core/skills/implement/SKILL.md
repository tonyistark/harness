---
name: implement
description: Implement one task from the task list inside its own worktree, test-first, touching only the task's files. Use for each task of the implement phase.
---

# Implement one task

Input: the task (id, title, files, requirements) in the run context. You are in a git
worktree that only this task uses.

1. Read the requirements the task cites and the relevant existing code.
2. Write or extend tests that express the requirement, run them, watch them fail.
3. Implement the smallest change that makes them pass. Stay inside the task's files; if you
   must touch another file, stop and report it instead.
4. Run the project's build and tests. Fix what you broke.
5. Do not commit; the harness commits with lineage trailers.

If the task cannot be completed as specified, say exactly why and what the spec should say
instead.

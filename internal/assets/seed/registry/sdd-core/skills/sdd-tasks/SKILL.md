---
name: sdd-tasks
description: Break an approved design into small, dependency-ordered tasks with file boundaries and requirement links. Use after design, before implementation.
---

# SDD tasks

Input: requirements and design for this run.

1. Create tasks of 2–15 minutes of focused work each. Each task names its files
   (no two parallel tasks share a file), the requirement IDs it satisfies, and the tasks it
   depends on.
2. Assign a class: `implement` for logic and tests, `mechanical` for renames, docs and
   generated code.
3. Order tasks so independent ones can run in parallel.
4. Write `specs/<ticket>/tasks.md` and return the structured output.

Every requirement must be covered by at least one task.

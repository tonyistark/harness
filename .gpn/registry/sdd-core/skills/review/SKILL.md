---
name: review
description: Review one task's diff against its requirements as an independent reviewer and return pass or changes_requested with findings. Use after implementation, in a fresh session.
---

# Independent review

You did not write this code. Input: the task, its requirements, and the diff in the
worktree.

Check, in order: does the change satisfy each cited requirement; are there tests that would
fail if it did not; does anything outside the task's files change; any security issue
(secrets, injection, unsafe deserialization, missing authorization); any protected path.

Return `verdict: pass` only when every check passes. Otherwise return
`changes_requested` with findings that name file, line, severity and the fix.

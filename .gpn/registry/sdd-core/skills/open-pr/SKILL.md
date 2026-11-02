---
name: open-pr
description: Open the pull request for a finished run with the run ID, requirement IDs, task list and gate results in the body. Use as the final step of the feature workflow.
---

# Open the pull request

Input: run ID, requirements, tasks, gate results (in the run context).

Use the GitHub connector to open one pull request from the run's branch to the default
branch. The body must contain: the Jira item, the run ID (`GPN-Run: ...`), every
requirement ID with its text, the task list, and the gate results table. Do not merge.

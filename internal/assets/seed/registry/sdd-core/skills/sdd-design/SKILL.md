---
name: sdd-design
description: Produce a design from approved requirements: components with file boundaries, contracts, and risks, each linked to requirement IDs. Use after requirements are approved.
---

# SDD design

Input: the approved requirements for this run (in the run context).

1. Propose the smallest set of components that satisfies every requirement. For each,
   name the files it owns (existing or new) and the requirement IDs it serves.
2. State the contracts between components (types, function signatures, messages).
3. List risks: what could make this design fail its requirements, and how we would know.
4. Write `specs/<ticket>/design.md` and return the structured output.

Do not write implementation code in this step. Every component must cite at least one
requirement; a component that serves none is scope creep.

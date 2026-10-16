---
name: speckit-superpowers-execute
description: Import this feature's tasks.md into beads and execute it with the superpowers workflow
compatibility: Requires spec-kit project structure with .specify/ directory
metadata:
  author: Anton Krivenko
  source: superpowers:commands/execute.md
---

Invoke the `superpowers:using-spec-kit` skill with the Skill tool and follow it from its
**2. Import** step, for the feature directory in `$ARGUMENTS` if one is given, else the active
feature in `.specify/feature.json`. Do not run `/speckit-implement`: superpowers is this repo's
only executor.
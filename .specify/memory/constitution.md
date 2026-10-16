# tgshd Constitution

## Core Principles

### I. Idiomatic Go, Standard Library First

- Code MUST be idiomatic Go and follow the existing patterns of the file being edited.
- The standard library MUST be the first choice; a third-party package is used only when the
  standard library is insufficient (see Principle III).
- Errors MUST be wrapped with `%w` when they cross a function boundary with added context, and
  MUST NOT be silently discarded.
- Packages MUST stay focused on one responsibility; business logic sits behind small interfaces
  and the concrete implementations are thin adapters over external systems.

Rationale: a small daemon stays maintainable only if every package reads the same way and the
dependency surface stays minimal.

### II. Test-First, Standard Library Testing (NON-NEGOTIABLE)

- Every behaviour change MUST be developed test-first: the test is written, seen to fail, then
  made to pass.
- Tests MUST use only the standard library `testing` package, table-driven where it helps, with
  hand-written fakes for interfaces. testify, mock generators and any other test dependency are
  forbidden.
- Decision logic MUST be pure (no I/O) and live in packages inside the coverage gate; I/O MUST be
  confined to thin adapters behind small source interfaces.
- The coverage gate measures the `COVERPKG` scope; total coverage over it MUST stay at or above
  80%. `COVERPKG` in `Taskfile.yml` and `-coverpkg` in `.github/workflows/ci.yml` MUST be kept
  identical. Adapters that only run against real hardware or services are excluded from the gate
  but their tests still run.

Rationale: pure, table-tested logic is what makes alerting and command behaviour trustworthy
without the real hardware present.

### III. Dependency Discipline

- A new third-party dependency (direct, or an indirect one promoted to direct) MUST be approved
  by the maintainer before it is added. The request MUST state the package, what it solves and
  why the standard library is insufficient.
- Only well-known, popular and actively maintained packages are eligible.
- Approved dependencies MUST be recorded in the feature plan's Technical Context.

Rationale: every dependency is code shipped in a root-run daemon; it must earn its place.

### IV. Static, CGO-Free Builds

- Builds MUST succeed with `CGO_ENABLED=0`; no dependency may require cgo.
- The Docker image is distroless and static; nothing may assume a libc or a shell inside it.

Rationale: a single static binary deploys anywhere and keeps the image minimal.

### V. Optional Features Are Env-Gated; the Daemon Always Starts

- A feature that needs external hardware or services MUST be wired only when configured, through
  an `xxxProviders()` `fx.Option` gated by an environment-variable check in `main()`.
- The daemon MUST start and keep running when an optional dependency is absent or unreachable
  (network, Telegram, Docker, PostgreSQL, the state store); the affected feature degrades and
  reports the problem instead of stopping the process.

Rationale: a monitoring daemon that dies when what it monitors is down is worthless.

### VI. Public Repository Hygiene

- The repository is public. Code, docs, tests, fixtures, specs, commit messages and tracker data
  MUST NOT contain real host data: hostnames, IP addresses (use RFC 5737 / `example.org`),
  serial numbers, device models from a real host, phone numbers (use `+10000000000`-style),
  container names, URLs, or personal data other than the maintainer's public git identity.
- Nothing in the repository may reference private repositories.
- Unpushed changes MUST be scanned for private data before they are pushed.

Rationale: once published, data cannot be reliably withdrawn.

### VII. Commit and Workflow Discipline

- Commit subjects MUST be plain, capitalized, imperative sentences (e.g. `Add gammu SMS
  builder`) with no conventional-commit prefixes and no attribution lines.
- Work is committed directly on `main`.
- Pushing to the shared remote happens only on the maintainer's explicit request.

Rationale: a linear, readable history on a single branch suits a single-maintainer project.

## Engineering Constraints

- Language and runtime: Go (current toolchain in `go.mod`), uber/fx composition root in
  `cmd/main.go`, gocron for workers, zap for logging, telebot for Telegram.
- Telegram commands are accepted only from the configured chat and allow-list.
- User-facing messages MUST respect transport limits (Telegram 4096 UTF-16 units, SMS GSM-7/UCS-2
  segmenting) and MUST NOT leak secrets (tokens, URL credentials, headers).
- Host-specific configuration lives outside the repository (environment variables and the health
  YAML file); the repository ships only anonymised examples.

## Development Workflow & Quality Gates

- Features are specified with Spec Kit (`/speckit-specify` → `/speckit-clarify` →
  `/speckit-plan` → `/speckit-tasks` → `/speckit-analyze`) and executed with superpowers
  (`/speckit-superpowers-execute`); `/speckit-implement` is not used.
- Every plan MUST include a Constitution Check against Principles I–VII; a violation MUST be
  justified in the plan's Complexity Tracking or removed.
- Before any commit lands: `task lint` MUST report 0 issues and `task test:coverage` (race
  detector on) MUST pass with the 80% floor met. `CGO_ENABLED=0` builds MUST succeed.
- Each change is reviewed (spec compliance and code quality) before it lands on `main`.

## Governance

- This constitution supersedes other practice documents. `.claude/CLAUDE.md` is the runtime
  guide for agents and developers and MUST stay consistent with it; on conflict the constitution
  wins and the guide is corrected.
- Amendments are made with `/speckit-constitution`, committed with a plain imperative subject, and
  versioned semantically: MAJOR for removing or redefining a principle, MINOR for a new principle
  or materially expanded guidance, PATCH for clarifications.
- Plan Constitution Checks, task reviews and `/speckit-converge` treat MUST statements here as
  binding; a MUST violation is a critical finding.

**Version**: 1.0.0 | **Ratified**: 2026-10-16 | **Last Amended**: 2026-10-16

# Implementation Plan: Telegram Topic Routing

**Branch**: `004-telegram-topic-routing` (work lands on `main`) | **Date**: 2026-10-31 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `/specs/004-telegram-topic-routing/spec.md`

## Summary

tgshd posts each Telegram message to one of its three forum topics (alerts, SMS, system) with a
per-message sound setting, falls back to General whenever a topic is missing or rejected, accepts
commands from a forum supergroup only from allowed users, and answers every command silently in
the topic it came from.

**Approach (chosen in brainstorming).** **A — every send carries an explicit destination**
(chat, thread, silent). The publisher and handler signatures change from a bare recipient id to a
destination value; handlers receive the command's origin and reply to it; health messages carry a
route (topic + sound) that defaults by kind and is persisted with queued retries; the
thread-rejection fall-back lives once, in a pure helper used by the Telegram publisher.
Trade-off: about 20 mechanical call-site edits, accepted because a missed spot becomes a compile
error and every routing case is a table test. Rejected: **B** — destination in
`context.Context` (hidden; a forgotten spot silently lands in General; against the repo's style);
**C** — one publisher per topic (alerts still need per-message sound and replies the originating
thread, so two mechanisms). Details: [research.md](research.md).

## Technical Context

**Language/Version**: Go 1.27 (`go.mod`)

**Primary Dependencies**: existing only — uber/fx, zap, telebot v4 (`SendOptions.ThreadID`,
`SendOptions.DisableNotification`, `Message.ThreadID`, `Message.TopicMessage`), gocron. **No new
dependency.**

**Storage**: SQLite state store — one additive migration: `queue.route TEXT NOT NULL DEFAULT ''`.

**Testing**: standard library `testing`, table-driven, hand-written fakes.

**Target Platform**: Linux server, static `CGO_ENABLED=0` binary, distroless image.

**Project Type**: single Go daemon.

**Performance Goals**: none new; one extra Telegram call only on a rejected thread.

**Constraints**: public repository — no real chat ids, topic ids or group names anywhere; messages
keep the 4096-unit limit.

**Scale/Scope**: one chat, three topics; ~20 call sites change signature.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | Status |
|---|---|
| I. Idiomatic Go, stdlib first | Pass — small value types, errors wrapped with `%w`, focused new package. |
| II. Test-first, stdlib testing, pure logic in gate | Pass — routing, access, topic parsing and the fall-back are pure in `pkg/telegram/route`, added to `COVERPKG` in `Taskfile.yml` **and** `.github/workflows/ci.yml`; telebot glue stays a thin adapter. |
| III. Dependency discipline | Pass — none added. |
| IV. CGO-free | Pass. |
| V. Env-gated, daemon always starts | Pass — topic settings optional; invalid values warn and fall back, never stop startup. |
| VI. Public repository hygiene | Pass — docs and tests use placeholder ids (e.g. `-1000000000000`, thread `2`); no group names. |
| VII. Commit discipline | Pass — plain imperative subjects, on `main`. |

Post-design re-check: Pass. The store migration is additive and old rows load unchanged.

## Project Structure

### Documentation (this feature)

```text
specs/004-telegram-topic-routing/
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── contracts/
│   ├── config.md
│   └── telegram.md
└── tasks.md             # /speckit-tasks
```

### Source Code (repository root)

```text
pkg/telegram/route/          # NEW, pure, in COVERPKG
├── route.go                 # Destination, Topic, Route (+ text form), Topics, ParseTopics, Resolve
├── origin.go                # Origin, Reply
├── access.go                # AccessList (moved from pkg/telegram), ChatKind, Permit
└── fallback.go              # ThreadRejected, SendWithFallback

pkg/telegram/
├── consumer.go              # Handler.Handle(ctx, route.Origin, cmd); origin from message
│                            #   (thread only if TopicMessage); Permit by chat kind;
│                            #   channel-post handler strips @<bot>
├── publisher.go             # Publish(ctx, route.Destination, msg) with ThreadID,
│                            #   DisableNotification, via route.SendWithFallback
└── access.go                # removed (moved)

pkg/command/                 # Publisher.Publish(ctx, route.Destination, msg);
                             #   every handler replies to origin.Reply();
                             #   /sms requires origin.ChatID == configured chat;
                             #   Script.Execute(ctx, cmd, threadID)
pkg/script/script.go         # TELEGRAM_THREAD_ID in env when threadID > 0
pkg/sms/worker.go, inbox.go  # Publisher takes a Destination; worker holds the SMS destination (loud)

pkg/health/notify/           # Message.Route / QueuedMessage.Route (zero = default by kind);
                             #   TelegramSender.Send(ctx, route.Route, text);
                             #   checker recovered → alerts/silent;
                             #   backlog prefix only on alerts routes, sent loud
pkg/health/runner/           # Push variant carrying a route (weekly summary → system/silent)
pkg/health/store/            # migration: queue.route; read/write/validate

cmd/main.go                  # TELEGRAM_TOPIC_* config, ParseTopics + startup warnings,
                             #   topics into fx
cmd/health_adapters.go       # telegramSender resolves Route → Destination
cmd/health_reports.go,       # cmd-level handlers take route.Origin;
cmd/health_hardware.go       #   weekly summary pushed with system/silent
cmd/(sms wiring)             # SMS destination for modem and gammu workers

Taskfile.yml, .github/workflows/ci.yml   # COVERPKG += ./pkg/telegram/route (identical)
README.md, .env.dist, .claude/CLAUDE.md  # topic settings, routing table, group access rule,
                                         #   package map
```

**Structure Decision**: single Go module; the new pure package sits under `pkg/telegram/` because
it describes Telegram destinations, but it imports nothing internal, so `pkg/command`, `pkg/sms`,
`pkg/health/notify` and `pkg/telegram` all depend on it without cycles.

## Complexity Tracking

No constitution violations.

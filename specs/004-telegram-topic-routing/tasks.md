---

description: "Task list for Telegram topic routing"
---

# Tasks: Telegram Topic Routing

**Input**: Design documents from `/specs/004-telegram-topic-routing/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md

**Tests**: Every task is implemented test-first (Constitution II): each task writes its own tests,
standard library `testing` only, table-driven with hand-written fakes. There are no standalone
test tasks.

**Public repository**: tests, docs and examples use placeholder ids only (chat `-1000000000000`,
threads `2`, `3`, `4`, users `100`, `200`); no real group, chat or topic names.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel (different files, no dependency on an incomplete task)
- **[Story]**: US1, US2, US3 from spec.md

---

## Phase 1: Foundational (Blocking Prerequisites)

**Purpose**: the pure routing package and the switch of every send to an explicit destination.
Each task leaves the build and all tests green. Behaviour stays as today except that command
replies become silent.

- [x] T001 [P] Create the pure package pkg/telegram/route/route.go per data-model.md: `Destination{ChatID int64; ThreadID int; Silent bool}` ("ThreadID 0 = no topic"), `Topic` (`TopicNone`, `TopicAlerts`, `TopicSMS`, `TopicSystem`), `Route{Topic; Silent}` with `String()`/`ParseRoute` over exactly the text forms `""`, `alerts`, `alerts/silent`, `sms`, `system/silent` ("Any other value is a load error"), `Topics` with `ParseTopics(alerts, sms, system string) (Topics, []string)` ("all three empty → no warning"; "empty while another is set" or "not a positive whole number" → thread 0 plus one warning naming `TELEGRAM_TOPIC_ALERTS`/`_SMS`/`_SYSTEM`) and `Resolve(chatID int64, r Route) Destination`; add `./pkg/telegram/route` to `COVERPKG` in Taskfile.yml and to `-coverpkg` in .github/workflows/ci.yml, keeping both identical (tests in pkg/telegram/route/route_test.go) (FR-002, FR-003, FR-004, R2, R11)
- [x] T002 [P] Add `Origin{ChatID int64; ThreadID int; SenderID int64}` with `Reply() Destination` (`{ChatID, ThreadID, Silent: true}`) in pkg/telegram/route/origin.go, and move `AccessList` (`NewAccessList`, `Empty`, `Allowed`) unchanged from pkg/telegram/access.go to pkg/telegram/route/access.go with its tests (pkg/telegram/route/origin_test.go, pkg/telegram/route/access_test.go); update pkg/telegram/consumer.go and cmd/main.go to the new import and delete pkg/telegram/access.go and pkg/telegram/access_test.go (FR-016, R2)
- [x] T003 Publishers take a destination: change `command.Publisher.Publish(ctx, route.Destination, msg string)` in pkg/command/entity.go and `sms.Publisher` in pkg/sms/worker.go to the same signature; pkg/telegram/publisher.go sends to `d.ChatID` with `SendOptions{ParseMode: telebot.ModeHTML, ThreadID: d.ThreadID, DisableNotification: d.Silent}`; handlers keep receiving `senderID` and reply to `route.Destination{ChatID: senderID, Silent: true}` in every pkg/command/*.go handler and in cmd/health_reports.go and cmd/health_hardware.go; pkg/sms/worker.go, pkg/sms/inbox.go and `telegramSender` in cmd/health_adapters.go hold `route.Destination{ChatID: cfg.ChatID}` (loud, no thread); update every fake publisher (pkg/command/*_test.go, pkg/sms/*_test.go, cmd/*_test.go) to record and assert the destination, replies silent (FR-005 replies, R1, R3)
- [x] T004 Handlers take an origin: change `telegram.Handler.Handle(ctx, route.Origin, cmd string)` in pkg/telegram/consumer.go, which builds the origin from the message ("ThreadID = msg.ThreadID only when msg.TopicMessage is true, else 0"; `SenderID` from `message.Sender`, 0 when absent as for channel posts; `ChatID` from `message.Chat`); every handler in pkg/command/*.go and the cmd-level handlers in cmd/health_reports.go and cmd/health_hardware.go reply to `origin.Reply()`; `/sms` in pkg/command/command_sms.go keeps today's rule by comparing `origin.ChatID` (not the sender) with the configured chat; update handler tests in pkg/command/*_test.go and cmd/*_test.go to pass origins and assert replies go to the origin chat and topic (FR-001, FR-015, FR-016, R1, R5)

**Checkpoint**: builds, all tests pass, replies are silent and go to the origin chat and topic; nothing pushed uses topics yet.

---

## Phase 2: User Story 1 - Alerts land in the alerts topic, and only problems ring (Priority: P1) 🎯 MVP

**Goal**: every message tgshd pushes lands in its topic with the sound from the routing table, also after a queued retry.

**Independent Test**: with the three topics configured, trigger a problem and its recovery, a checker error and its recovery, an incoming SMS and the weekly summary; check topic and sound of each, and of a PROBLEM retried from the queue.

- [x] T005 [US1] Route health messages in pkg/health/notify: add `Route route.Route` to `Message` and `QueuedMessage` (copied at enqueue); zero route resolves by kind ("problem → alerts loud, resolved → alerts silent, event → alerts loud"); replace `Options.Telegram` with a `TelegramSender` interface `Send(ctx, route.Route, text) error` (SMS keeps `Sender`); first sends and `Flush` retries pass the message's resolved route; the "Undelivered earlier:" prefix is attached only to a message whose route topic is alerts, and a message carrying it is sent loud; `CheckerErrors.Recovered` in pkg/health/notify/checker.go sends with `alerts/silent`; adapt `telegramSender` in cmd/health_adapters.go to the new interface, still sending to `route.Destination{ChatID: cfg.ChatID}` with the route's silent flag (tests in pkg/health/notify/notify_test.go, checker_test.go, channels_test.go: default per kind, explicit route kept, retry keeps route, prefix skipped on a system route and forces loud) (FR-005, FR-006, FR-007, R7, R8, R9)
- [x] T006 [US1] Persist the route: migration `ALTER TABLE queue ADD COLUMN route TEXT NOT NULL DEFAULT '';` in pkg/health/store/store.go, read and written with the other queue columns and validated with `route.ParseRoute` in pkg/health/store/state.go ("Any other value is a load error, like an unknown queue kind today"); a row from before the migration loads with an empty route (tests in pkg/health/store/state_test.go, store_test.go) (FR-007, R7)
- [x] T007 [US1] Push with a route: add a new method `Runner.PushRoute(ctx context.Context, check, key, text, record string, r route.Route)` in pkg/health/runner/runner.go — an event delivered with route `r`, logged in the alert log like `Push` — and push the weekly summary through it with `route.Route{Topic: route.TopicSystem, Silent: true}` from cmd/health_reports.go (tests in pkg/health/runner/runner_test.go: `TestPushRouteKeepsRoute`, `TestPushRouteQueuesWhenTelegramFails`; cmd/health_reports_test.go: summary pushed with `system/silent`) (FR-005, R7)
- [x] T008 [US1] Wire the topics in cmd: read `TELEGRAM_TOPIC_ALERTS`, `TELEGRAM_TOPIC_SMS`, `TELEGRAM_TOPIC_SYSTEM` as strings in the config in cmd/main.go, call `route.ParseTopics` once at startup and log each returned warning once, provide `route.Topics` through fx; `telegramSender` in cmd/health_adapters.go resolves each route with `Topics.Resolve(cfg.ChatID, r)`; the modem SMS worker and the gammu inbox worker get `Topics.Resolve(cfg.ChatID, route.Route{Topic: route.TopicSMS})` (loud) in their wiring in cmd (tests in cmd/main_test.go, cmd/health_test.go: destination per route, warnings logged once, none when all unset) (FR-001, FR-002, FR-003, FR-004, FR-005, FR-006, FR-009)

**Checkpoint**: User Story 1 works end to end with a configured group; without topic settings messages still arrive in the chat, with the new sound settings.

---

## Phase 3: User Story 2 - Commands work in the group, only for allowed people (Priority: P1)

**Goal**: commands from the group run only for allowed users and are answered silently where they were typed; the custom script learns the thread.

**Independent Test**: from an allowed user send commands in a topic, in General, as a reply in General and as `/cmd@<bot>`; from another member send the same; check what runs and where replies land; run a custom-script command from a topic and from General.

- [x] T009 [US2] Group access rule: add `ChatKind` (`ChatChannel`, `ChatPrivate`, `ChatGroup`) and `AccessList.Permit(kind ChatKind, chatID, senderID int64) bool` in pkg/telegram/route/access.go ("Channel → chat allowed; Private → chat allowed; Group (group, supergroup) → chat allowed and sender allowed"); pkg/telegram/consumer.go maps `telebot.ChatChannel`/`telebot.ChatChannelPrivate` → `ChatChannel`, `telebot.ChatPrivate` → `ChatPrivate`, `telebot.ChatGroup`/`telebot.ChatSuperGroup` → `ChatGroup` and drops refused updates with one warning carrying chat and sender id, no reply (tests in pkg/telegram/route/access_test.go) (FR-001, FR-010, FR-012, FR-013, FR-014, R6)
- [x] T010 [US2] Bot-name commands in channel posts: add `CommandName(text, botUsername string) (string, bool)` in pkg/telegram/route/command.go (`/name` → `/name`; `/name@<bot>` matching case-insensitively → `/name`; `/name@<other bot>` → not ours) and use it in the channel-post handler in pkg/telegram/consumer.go; group messages keep telebot's own routing, which already strips a matching `@<bot>` (tests in pkg/telegram/route/command_test.go) (FR-011, R3)
- [x] T011 [P] [US2] `/sms` origin rule in pkg/command/command_sms.go: accept only when `origin.ChatID` equals the configured `TELEGRAM_CHAT_ID` (any topic or General) and the update passed access; from any other chat, including an allowed private chat, ignore as today (tests in pkg/command/command_sms_test.go: group topic, group General, private chat of an allowed user, channel) (FR-018)
- [x] T012 [P] [US2] Custom script thread id: change `command.Script.Execute(ctx, cmd string, threadID int)` in pkg/command/command_unknown.go (pass `origin.ThreadID`, acknowledge "Script succeeded!" to `origin.Reply()`) and in pkg/script/script.go set `cmd.Env = append(os.Environ(), "TELEGRAM_THREAD_ID=<id>")` only when `threadID > 0`, leaving `Env` nil otherwise; argv unchanged (tests in pkg/command/command_unknown_test.go and pkg/script/script_test.go with a script that writes its environment to a temp file) (FR-016, FR-017, R10)

**Checkpoint**: User Stories 1 and 2 work independently.

---

## Phase 4: User Story 3 - Nothing is lost while topics are missing or wrong (Priority: P2)

**Goal**: a closed, deleted or unknown topic never loses a message.

**Independent Test**: send to a destination whose thread Telegram rejects; the message is resent once without the thread, keeping its sound; other errors are returned unchanged. (Missing and invalid settings are covered by T001 and T008; channel commands by T009.)

- [ ] T013 [US3] Thread fall-back: add `ThreadRejected(err error) bool` (case-insensitive match of `message thread not found`, `TOPIC_CLOSED`, `TOPIC_DELETED` in the error text) and `SendWithFallback(ctx, d Destination, send func(context.Context, Destination) error, warn func(threadID int, err error)) error` ("only when d.ThreadID != 0 and ThreadRejected: retry once with ThreadID 0, Silent unchanged; any other error is returned as is") in pkg/telegram/route/fallback.go, and use it from pkg/telegram/publisher.go with a zap warning carrying the thread id (tests in pkg/telegram/route/fallback_test.go: each rejection text, other errors, no thread, second failure returned) (FR-008, R4)

**Checkpoint**: all user stories work independently.

---

## Phase 5: Polish & Cross-Cutting Concerns

- [ ] T014 [P] Document the feature generically in README.md and .env.dist: the three `TELEGRAM_TOPIC_*` settings and their fall-back, the routing table (topic and sound per message), the group access rule (sender must be in `TELEGRAM_ALLOWED_IDS`), `/sms` limited to `TELEGRAM_CHAT_ID`, replies in the originating topic, `TELEGRAM_THREAD_ID` for the custom script, and that the bot must be an administrator of the group; placeholder ids only (FR-019)
- [ ] T015 [P] Update .claude/CLAUDE.md: package map row for `pkg/telegram/route`, the `telegram.Handler` signature with `route.Origin`, and `./pkg/telegram/route` in the coverage-gate list
- [ ] T016 Run quickstart.md's automated checks — `task lint` (0 issues), `task test:coverage` (80 % floor), `CGO_ENABLED=0 task build` — and scan the unpushed diff for real chat ids, topic ids, group names, hostnames and phone numbers (Constitution II, IV, VI)

---

## Dependencies & Execution Order

- **Phase 1**: T001 and T002 in parallel; T003 after both; T004 after T003 (same handler files).
- **Phase 2 (US1)**: after Phase 1. T005 → T006 (needs `QueuedMessage.Route`) → T007 → T008.
- **Phase 3 (US2)**: after Phase 1; independent of US1. T009 → T010 (both edit pkg/telegram/consumer.go); T011 and T012 in parallel with them.
- **Phase 4 (US3)**: after Phase 1; independent of US1 and US2.
- **Phase 5**: after all stories. T014 and T015 in parallel, T016 last.

## Parallel Example

```text
Phase 1:  T001 (route.go)         ∥ T002 (origin.go, access.go)
Phase 3:  T009 → T010 (consumer)  ∥ T011 (command_sms.go) ∥ T012 (command_unknown.go, script.go)
Phase 5:  T014 (README, .env.dist) ∥ T015 (CLAUDE.md)
```

## Implementation Strategy

1. Phase 1, then US1 (Phase 2): this is the MVP — alerts in their topics with the right sound.
2. US2 (Phase 3): commands from the group, safely.
3. US3 (Phase 4): the fall-back for rejected topics.
4. Polish and the full verification run.

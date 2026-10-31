# Research: Telegram Topic Routing

No `NEEDS CLARIFICATION` remained in the Technical Context; the items below record the decisions
taken while reading the code.

## R1. How a destination travels (approach)

- **Decision**: every send carries an explicit destination value — chat, thread (0 = none) and
  silent. `command.Publisher`, `sms.Publisher` and `telegram.Handler` take it instead of a bare
  recipient id; command handlers receive the command's origin and reply to `origin.Reply()`.
- **Rationale**: chosen by the maintainer in brainstorming. Nothing is implicit, so a missed call
  site is a compile error, and every routing case is a table test.
- **Alternatives considered**: (B) destination in `context.Context` — fewer edits, but hidden: a
  forgotten spot silently posts to General, and it goes against the repo's style of passing what
  is needed. (C) one publisher per topic — fine for SMS and the summary, but alerts still need a
  per-message sound and replies the originating thread, so it would be two mechanisms. Trade-off
  accepted: about 20 mechanical call-site edits.

## R2. Where the pure routing logic lives

- **Decision**: a new pure package `pkg/telegram/route`: `Destination`, `Topic`, `Route`, `Topics`
  (parsing the three settings and resolving a route), `Origin`, the access decision (`AccessList`
  moves here from `pkg/telegram/access.go`) and the thread-rejection fall-back
  (`SendWithFallback`, `ThreadRejected`). It joins `COVERPKG` (Taskfile and CI kept identical).
- **Rationale**: Constitution II — decision logic is pure and inside the coverage gate;
  `pkg/telegram` stays a thin telebot adapter outside it. `pkg/telegram/route` imports nothing
  internal, so `pkg/command`, `pkg/sms`, `pkg/health/notify` and `pkg/telegram` can all use it
  without cycles.
- **Alternatives considered**: types in `pkg/command` — would make `pkg/health/notify` and
  `pkg/telegram` depend on the command package; logic in `pkg/telegram` — outside the gate.

## R3. Library support (no new dependency)

- **Decision**: use telebot v4 as is: `SendOptions.ThreadID`, `SendOptions.DisableNotification`,
  `Message.ThreadID`, `Message.TopicMessage`. Command routing already strips `@<bot name>` when it
  matches `bot.Me.Username` (`update.go`), for messages in groups as well.
- **Rationale**: Constitution III; all needed fields exist in the pinned version.
- **Note**: the generic channel-post handler matches `fields[0]` itself, so it must strip a
  matching `@<bot name>` the same way.

## R4. Recognising a rejected thread

- **Decision**: telebot has no typed errors for topics; `ThreadRejected(err)` matches the Bot API
  description case-insensitively for `message thread not found`, `TOPIC_CLOSED` and
  `TOPIC_DELETED`. Only then, and only when the destination had a thread, the send is retried once
  with thread 0, keeping silent; a warning is logged with the thread id.
- **Rationale**: FR-008; any other error (network, rate limit) keeps today's handling.

## R5. Topic of a command

- **Decision**: `Origin.ThreadID = msg.ThreadID` only when `msg.TopicMessage` is true, else 0.
- **Rationale**: FR-015 — a reply inside General carries a `message_thread_id` that is not a
  topic.

## R6. Who may run a command

- **Decision**: `AccessList.Permit(kind, chatID, senderID)`: channel → chat allowed (no sender);
  private → chat allowed (chat id = user id); group/supergroup → chat allowed **and** sender
  allowed. The allow-list stays `TELEGRAM_CHAT_ID` ∪ `TELEGRAM_ALLOWED_IDS`. `/sms` additionally
  requires `origin.ChatID == TELEGRAM_CHAT_ID` (FR-018).
- **Rationale**: FR-012–FR-014, the `/sms` clarification. Today `/sms` compares the *sender* with
  the chat id; in a group the sender is the user, so that check moves to the origin chat.

## R7. Routing health messages, including retries

- **Decision**: `notify.Message` gains `Route` (`route.Route{Topic, Silent}`). Its zero value
  means "default for the kind": problem → alerts loud, resolved → alerts silent, event → alerts
  loud. Explicit routes: checker recovered → alerts silent; weekly summary → system silent. The
  route is persisted with a queued message in a new `queue.route` column (migration, default `''`
  = default for the kind), so a retry goes where the first send would have.
- **Rationale**: checker error and checker recovered are both `event` with the same key, and the
  weekly summary is an `event` too; only an explicit, persisted route tells them apart without
  parsing text. This replaces the brainstorming expectation of "no schema change": one additive
  column, old rows keep working (empty route → default for the kind).
- **Alternatives considered**: derive from the label in the text (fragile); new queue kinds
  (ripples into the alert log and its schema constraint).

## R8. The "Undelivered earlier:" backlog

- **Decision**: the backlog is a prefix the notifier puts ahead of the next Telegram message. It is
  attached only to a message routed to the alerts topic, and a message carrying it is sent loud.
- **Rationale**: the backlog is alert material (routed alerts, loud) and must not ride on a silent
  RESOLVED or on the weekly summary in the system topic.

## R9. Telegram sender interface in the notifier

- **Decision**: `notify.Options.Telegram` becomes a `TelegramSender` with
  `Send(ctx, route.Route, text) error`; `notify.Sender` stays for SMS. The cmd adapter resolves the
  route through `route.Topics` into a `route.Destination` on `TELEGRAM_CHAT_ID` and calls the
  publisher.

## R10. Custom script thread id

- **Decision**: `command.Script.Execute(ctx, cmd string, threadID int)`; `pkg/script` sets
  `cmd.Env = append(os.Environ(), "TELEGRAM_THREAD_ID=<id>")` only when `threadID > 0`; argv
  unchanged.
- **Rationale**: FR-017; a nil `Env` keeps today's inherited environment byte for byte.

## R11. Startup warnings for topic settings

- **Decision**: `route.ParseTopics(alerts, sms, system string) (Topics, []string)` returns the
  warnings; cmd logs each once at startup. No warning when all three are empty (FR-003).

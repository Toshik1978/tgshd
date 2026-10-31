# Feature Specification: Telegram Command Menu

**Feature Branch**: `005-telegram-command-menu`

**Created**: 2026-10-31

**Status**: Draft

**Input**: User description: "Automatic Telegram bot command menu publishing: tgshd publishes its
own command menu at startup so only allowed users see the commands that are actually enabled,
replacing the stale list set by hand in BotFather."

Today the bot's command menu (the list Telegram shows after typing `/`) was set by hand in
BotFather. It holds three stale commands, and Telegram shows it to everyone in every chat the bot
is in, including every topic of a forum group. tgshd knows exactly which commands it serves and
who may use them, so it takes over the menu: at every start it clears any hand-made list and
publishes the real one, only to the people on the allow-list.

Telegram cannot scope a menu to a single topic of a group; the narrowest group scope is one user
inside one group. This is accepted: an allowed user sees the menu in every topic of an allowed
group, and nobody else sees it at all.

## Clarifications

### Session 2026-10-31

- Q: When someone is taken off the allow-list, should tgshd clear the menu it published for them
  earlier? → A: No. Stale menus stay; the README documents how to clear one by hand.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Allowed users see the real commands (Priority: P1)

The operator opens a private chat with the bot, or a topic of the allowed group, types `/`, and
sees exactly the commands tgshd currently serves, each with a short description. Commands that
need parameters say so in their description (e.g. "Send SMS: /sms <phone> <text>").

**Why this priority**: this is the whole point: a menu that matches what the daemon actually
does, without maintaining it by hand.

**Independent Test**: start tgshd with an allow-list holding one user and one group; in both the
private chat and the group, `/` lists the enabled commands with their descriptions.

**Acceptance Scenarios**:

1. **Given** the allow-list holds user U, **When** tgshd starts and reaches Telegram, **Then** U's
   private chat with the bot shows the enabled commands with descriptions.
2. **Given** the allow-list holds user U and group G, **When** tgshd starts, **Then** U sees the
   menu in G, in every topic of G.
3. **Given** a feature is not configured (e.g. no modem), **When** tgshd starts, **Then** the
   commands of that feature are not in the menu.
4. **Given** a command takes parameters, **When** the user reads its menu entry, **Then** the
   description shows its usage; sending it bare from the menu replies with usage, as today.

---

### User Story 2 - Nobody else sees the commands (Priority: P1)

A member of the allowed group who is not on the allow-list, or anyone in any other chat, types
`/` and sees no tgshd commands. The three stale BotFather commands are gone everywhere.

**Why this priority**: the reported problem: commands visible to everyone in every topic.

**Independent Test**: after tgshd starts, a group member not on the allow-list and a user in a
chat not on the allow-list both see no bot commands.

**Acceptance Scenarios**:

1. **Given** a default menu exists (e.g. one set by hand in BotFather), **When** tgshd starts
   and reaches Telegram, **Then** the default menu is removed; with none set, the removal is a
   harmless no-op.
2. **Given** group G is allowed and member M is not on the allow-list, **When** M types `/` in
   G, **Then** no tgshd commands are offered.

---

### User Story 3 - Script commands appear with their own descriptions (Priority: P2)

The operator's custom script (`TELEGRAM_UNKNOWN_SCRIPT`) lists its commands with descriptions,
one per line in BotFather's own format `name - description`. Those commands appear in the menu
next to the built-in ones and keep working as before.

**Why this priority**: the script's commands are part of what the operator uses daily, but the
menu is already useful without them.

**Independent Test**: with a script printing two `name - description` lines, the menu shows
both with the script's descriptions, and sending either one runs the script as today.

**Acceptance Scenarios**:

1. **Given** the script prints `wake - Wake a PC: /wake <host>`, **When** tgshd starts, **Then**
   `/wake` is in the menu with description "Wake a PC: /wake <host>" and is handled by the
   script.
2. **Given** the script prints a line that is not a valid `name - description` entry, **When**
   tgshd starts, **Then** that line is skipped with a warning naming it, and the other lines
   still work.

---

### Edge Cases

- **Telegram unreachable at boot**: the daemon starts and serves nothing until Telegram answers,
  as today; the menu is published once Telegram first answers, without blocking startup.
- **Publishing fails for one destination** (e.g. a user who never opened a chat with the bot,
  a channel ID on the allow-list, a group the bot was removed from): that destination is
  skipped with a warning naming its ID; the others are still published, and command handling is
  unaffected.
- **Channel IDs on the allow-list**: channels and groups both have negative IDs; channels have
  no per-user menu, so publishing to one fails and is handled as above.
- **Empty allow-list**: the default menu is still removed; nothing is published; the existing
  "allow-list is empty" warning stands.
- **Allow-list holds groups but no users**: no group menus can be published (a group menu is
  per user); only the default menu is removed.
- **A user is removed from the allow-list**: their previously published menu stays visible to
  them (tgshd does not remember past destinations); their commands are still refused, as today.
  The README documents a one-off Telegram call the operator can run to clear it.
- **Script name clashes with a built-in command**: the built-in command wins; the script entry
  is skipped with a warning.
- **Duplicate script names**: the first entry wins; later ones are skipped with a warning.
- **More commands than Telegram allows in one menu (100)**: the first 100 are published, with a
  warning; all commands still work when typed.
- **Script prints the old `a|b|c` format**: the line is not a valid entry and is skipped with a
  warning; the operator updates the script (the old format is no longer supported).
- **Script prints blank lines**: ignored silently.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Every command tgshd serves MUST have a name and a one-line description; a handler
  serving several names (e.g. the custom script handler) MUST give each name its own entry.
- **FR-002**: The same list of names MUST drive both command handling and the published menu, so
  the two cannot disagree.
- **FR-003**: Descriptions of commands that take parameters MUST show their usage.
- **FR-004**: Once Telegram first answers after start, tgshd MUST remove the bot's default
  (every-chat) command menu.
- **FR-005**: tgshd MUST then publish the menu of enabled commands, in a stable order, to:
  - each allowed user ID (positive ID) in that user's private chat with the bot;
  - each allowed user inside each allowed group ID (negative ID), visible to that user only.
- **FR-006**: tgshd MUST NOT publish the menu to any other scope (all groups, all private chats,
  group administrators, or a whole group).
- **FR-007**: Publishing MUST happen once per start, MUST NOT delay or block startup, command
  handling or shutdown, and a failure for one destination MUST be logged with its ID and MUST
  NOT stop the rest. On shutdown, publishing stops before its next call.
- **FR-008**: The custom script, run with no arguments, MUST print one command per line as
  `name - description`; tgshd MUST route each listed name to the script and publish it with
  that description.
- **FR-009**: A script line that is not a valid entry (name not 1–32 of lowercase letters,
  digits and underscores, a leading `/` allowed and dropped; or description empty or longer than
  256 characters) MUST be skipped with a warning; blank lines are ignored.
- **FR-010**: The old single-line `a|b|c` script output MUST no longer be accepted as a command
  list.
- **FR-011**: A script name equal to a built-in command name, or repeated in the script, MUST be
  skipped with a warning (built-in and first entry win).
- **FR-012**: If more than 100 enabled commands exist, the first 100 MUST be published and a
  warning logged; all commands MUST still be handled.
- **FR-013**: Access rules for running commands MUST stay as they are; the menu only controls
  what is shown.
- **FR-014**: tgshd MUST NOT store past menu destinations; the README MUST document how the
  operator clears the menu of a user removed from the allow-list, and the new script output
  format.

### Key Entities

- **Bot command**: a command name (without `/`) and its one-line description.
- **Menu destination**: where Telegram shows a menu: a user's private chat, or one user inside
  one group. Derived from the allow-list.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After a start, the stale BotFather commands appear in 0 chats.
- **SC-002**: Allowed users see 100% of enabled commands, and 0 disabled ones, in their private
  chat and in every allowed group.
- **SC-003**: Group members not on the allow-list see 0 tgshd commands.
- **SC-004**: Adding, removing or enabling a command changes the menu on the next start with no
  manual BotFather step.
- **SC-005**: A boot with Telegram unreachable starts the daemon as quickly as today.

## Assumptions

- The allow-list (`TELEGRAM_CHAT_ID` + `TELEGRAM_ALLOWED_IDS`) is the single source of who sees
  the menu; positive IDs are users, negative IDs are groups (or channels).
- The menu is published in the default language only; no per-language menus.
- The script's command list is read once at start, as today; changes take effect on restart.
- Only the default menu is removed; tgshd does not try to find or clear menus it published to
  users who were later removed from the allow-list.
- The bottom-left Menu button needs no change: Telegram shows it in private chats once commands
  exist there.
- The operator updates their script to the new format; there is no compatibility period.

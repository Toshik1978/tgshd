# Feature Specification: Telegram Topic Routing

**Feature Branch**: `004-telegram-topic-routing`

**Created**: 2026-10-31

**Status**: Draft

**Input**: User description: "Telegram forum-topic routing: send tgshd's messages to topics of a forum supergroup (alerts, SMS, system), make routine messages silent, accept commands from the supergroup only from allowed users, and reply in the thread a command came from."

The operator moves tgshd's Telegram messages from a channel to a forum supergroup. A channel
cannot have topics; a forum supergroup can. tgshd then puts each message it sends into the
topic it belongs to, and decides per message whether it makes a sound, so the few messages that
need action are not drowned by routine ones.

tgshd only knows its own three topics: **alerts**, **SMS** and **system**. Whatever else the
operator's group holds is outside tgshd and outside this specification. Messages without a topic
land in the group's **General** area.

This feature amends the Telegram delivery of [001 Host Health Monitoring](../001-host-health-monitoring/spec.md),
[002 Deeper Health Checks](../002-deeper-health-checks/spec.md) and
[003 Memory Protection](../003-memory-protection/spec.md): what those features send is unchanged;
where it goes in the chat and whether it makes a sound is defined here. Email and SMS delivery
of alerts is unchanged.

## Clarifications

### Session 2026-10-31

- Q: Where should `/sms` work after the move? → A: Only in the configured chat (in the group: any topic or General), and in a group only from an allowed sender; not from a private chat.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Alerts land in the alerts topic, and only problems ring (Priority: P1)

The operator reads tgshd's messages in a forum supergroup. A host problem appears in the
alerts topic and the phone rings. When the problem resolves, the recovery appears in the same
topic without a sound. Incoming SMS appear in the SMS topic and ring. The weekly summary
appears in the system topic without a sound.

**Why this priority**: This is the reason for the change: action-worthy messages must stand out
and routine ones must stop interrupting.

**Independent Test**: Configure the chat and the three topic ids, trigger a problem, its
recovery, an incoming SMS and the weekly summary, and check where each lands and whether it
was sent with sound.

**Acceptance Scenarios**:

1. **Given** the alerts topic is configured, **When** a check opens a problem, **Then** the
   PROBLEM message is posted in the alerts topic with sound.
2. **Given** that problem is open, **When** it resolves, **Then** the RESOLVED message is posted
   in the alerts topic without sound.
3. **Given** a checker fails, **When** its CHECKER ERROR message is sent, **Then** it is posted
   in the alerts topic with sound; **When** the checker recovers, **Then** the recovery message is
   posted in the alerts topic without sound.
4. **Given** a fix action, a memory-guard action, a drive-retired notice, a delivery-channel
   failure notice or an "Undelivered earlier:" backlog is sent, **Then** it is posted in the
   alerts topic with sound.
5. **Given** the SMS topic is configured, **When** an SMS arrives through the gammu inbox or the
   modem, **Then** it is forwarded to the SMS topic with sound.
6. **Given** the system topic is configured, **When** the weekly summary is due, **Then** it is
   posted in the system topic without sound.
7. **Given** a PROBLEM failed to send and was queued, **When** it is retried later, **Then** it
   goes to the alerts topic with sound, exactly as it would have the first time.

---

### User Story 2 - Commands work in the group, only for allowed people (Priority: P1)

The operator types a command such as `/status` in any topic of the group, or in General. tgshd
runs it and replies, without sound, in the same place the command was typed. Other members of
the group can read everything, but their commands are ignored.

**Why this priority**: Without it the operator loses control of the daemon after the move, and
in a group any member could otherwise run commands that send SMS or change state.

**Independent Test**: From an allowed user, send commands in a topic, in General and with the
`/cmd@BotName` form; from another group member, send the same commands; check which run and
where the replies land.

**Acceptance Scenarios**:

1. **Given** the operator's user id is allowed, **When** the operator sends `/status` in the
   alerts topic, **Then** the reply is posted in the alerts topic without sound.
2. **Given** the same, **When** the operator sends `/status` in General, **Then** the reply is
   posted in General without sound.
3. **Given** the same, **When** the operator sends `/status@<bot name>`, **Then** it is handled
   exactly like `/status`.
4. **Given** another member whose user id is not allowed, **When** they send any command,
   including a custom-script command, **Then** nothing runs, nothing is replied, and one warning
   is logged.
5. **Given** the operator replies to a message inside General with a command, **When** tgshd
   answers, **Then** the reply is posted in General, not in a non-existent topic.
6. **Given** the operator runs a custom-script command from a topic, **When** the script runs,
   **Then** it receives that topic's thread id in its environment and its "Script succeeded!"
   acknowledgement is posted in that topic; **When** run from General, **Then** the thread id is
   not set and the acknowledgement is posted in General.

---

### User Story 3 - Nothing is lost while topics are missing or wrong (Priority: P2)

The operator deploys the new tgshd before the group exists, or mistypes a topic id, or later
closes or deletes a topic. Every message still arrives somewhere visible.

**Why this priority**: An alert lost to a configuration mistake is worse than an alert in the
wrong place; this is the safety net that makes the change safe to roll out.

**Independent Test**: Run with no topic ids, with an invalid topic id, and with a topic id that
Telegram rejects, and check that every message is delivered.

**Acceptance Scenarios**:

1. **Given** no topic id is configured at all, **When** any message is sent, **Then** it goes to
   the configured chat without a topic, with its sound setting applied, and no routing warning is
   logged.
2. **Given** the alerts topic id is empty or not a number while another topic id is set,
   **When** a PROBLEM is sent, **Then** it goes to General with sound, and a warning naming the
   setting is logged once.
3. **Given** a configured topic was deleted or closed, or its id does not exist, **When**
   Telegram rejects a message for that reason, **Then** the message is sent once more without a
   topic and lands in General.
4. **Given** the configured chat is still the old channel, **When** the operator posts a command
   in the channel, **Then** it is handled as it is today.

---

### Edge Cases

- A topic id that is set but Telegram rejects for another reason (network failure, rate limit):
  handled by the existing delivery retry, not by the fall-back to General.
- The fall-back to General itself fails: the message is a failed send and follows the existing
  delivery rules (queued and retried for alerts, error logged for replies).
- A reply to a command in a topic where the topic was deleted meanwhile: falls back to General
  like any other message.
- A command sent in a private chat with the bot by an allowed user: handled, replied in that
  private chat without sound.
- A command sent in a private chat by a user who is not allowed: ignored with one warning, as
  today.
- `/sms` keeps its stricter rule: it is accepted only when the command comes from the configured
  chat (the group, or the channel before the switch), from an allowed sender; it is not accepted
  from a private chat.
- Messages tgshd sends that have no topic of their own (none are expected beyond replies) go to
  General.
- A thread id carried by a message that is not a topic message (a reply thread in General) is
  never used as a topic and never passed to the custom script.

## Requirements *(mandatory)*

### Functional Requirements

**Configuration**

- **FR-001**: The configured chat (`TELEGRAM_CHAT_ID`) MUST be usable as a forum supergroup, as
  well as a channel or a private chat as today.
- **FR-002**: tgshd MUST read the thread ids of three topics from `TELEGRAM_TOPIC_ALERTS`,
  `TELEGRAM_TOPIC_SMS` and `TELEGRAM_TOPIC_SYSTEM`. Each is optional.
- **FR-003**: When none of the three is set, tgshd MUST send without a topic and MUST NOT log a
  routing warning.
- **FR-004**: When at least one is set, any of the three that is unset, empty or not a positive
  whole number MUST be treated as "no topic" for its messages, and tgshd MUST log one warning
  naming the setting at startup.

**Routing and sound**

- **FR-005**: tgshd MUST send each message to the configured chat, in the topic and with the
  sound setting below; "silent" means the message is delivered without a notification sound.

  | Message | Topic | Sound |
  |---|---|---|
  | PROBLEM | alerts | loud |
  | RESOLVED | alerts | silent |
  | CHECKER ERROR | alerts | loud |
  | Checker recovered | alerts | silent |
  | Fix action, memory-guard action, drive retired | alerts | loud |
  | Delivery-channel failure notice, "Undelivered earlier:" backlog | alerts | loud |
  | Incoming SMS (gammu inbox, modem) | SMS | loud |
  | Weekly summary | system | silent |
  | Reply to a command, custom-script acknowledgement | where the command came from | silent |

- **FR-006**: The sound setting MUST apply whether or not topics are configured.
- **FR-007**: A message queued after a failed send MUST be delivered later to the same topic and
  with the same sound setting it would have had at first.
- **FR-008**: When Telegram rejects a message because its topic does not exist, is closed or is
  deleted, tgshd MUST resend it once without a topic, keeping its sound setting.
- **FR-009**: Routing MUST NOT change what is sent by email or SMS.

**Commands**

- **FR-010**: tgshd MUST accept commands posted as ordinary messages in a group, in any topic or
  in General, as well as channel posts and private messages as today.
- **FR-011**: tgshd MUST accept the `/command@<bot name>` form for its own bot name, handled as
  `/command`.
- **FR-012**: In a group, a command MUST run only when the group is the configured or an allowed
  chat **and** the sender's user id is in `TELEGRAM_ALLOWED_IDS`. Commands from other members
  MUST be ignored without a reply, and one warning with the chat and sender id MUST be logged.
- **FR-013**: In a private chat, a command MUST run only when the chat is allowed (as today; a
  private chat's id is the user's id).
- **FR-014**: In a channel, where posts carry no sender, the allow-list check by chat MUST stay
  as today.
- **FR-015**: A command counts as coming from a topic only when Telegram marks the message as a
  topic message; otherwise it comes from General (or from a chat without topics).
- **FR-016**: Every reply to a command, including errors and usage texts, MUST be posted in the
  chat the command came from, in the topic it came from (or General), without sound. Replies
  MUST NOT be sent to the sender's private chat when the command came from a group.
- **FR-017**: When a command is handed to the custom script, the script MUST receive
  `TELEGRAM_THREAD_ID` set to the topic's thread id when the command came from a topic, and MUST
  NOT receive it otherwise. Its arguments MUST stay as today (one argument, the whole message).
- **FR-018**: `/sms` MUST be accepted only when the command comes from the configured chat
  (`TELEGRAM_CHAT_ID`, any topic or General) and passes FR-012 or FR-014; from any other chat,
  including a private chat with an allowed user, it MUST be ignored as today.

**Public repository**

- **FR-019**: The README and the example configuration MUST document the three topic settings,
  the routing table and the group access rule generically, without naming any real group, chat
  id or topic id.

### Key Entities

- **Destination**: where one message goes: the chat, the topic (or none, meaning General or a
  chat without topics), and whether it makes a sound.
- **Topic settings**: the three optional thread ids (alerts, SMS, system), resolved once at
  startup, each either valid or "no topic".
- **Command origin**: the chat, the topic (only for topic messages) and the sender of an
  incoming command; it decides whether the command runs and where its reply goes.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With topics configured, 100% of PROBLEM, CHECKER ERROR and incoming-SMS messages
  ring, and 0% of RESOLVED, checker-recovered, weekly-summary and command-reply messages ring.
- **SC-002**: Every message type in the routing table lands in its topic when that topic is
  configured and exists.
- **SC-003**: No message is lost to routing: with a missing, invalid, closed or deleted topic,
  100% of messages are still delivered to the chat.
- **SC-004**: 0 commands from group members who are not allowed are executed.
- **SC-005**: 100% of command replies land in the topic (or General) where the command was
  typed, and none in a private chat when the command came from a group.
- **SC-006**: A deployment with no topic settings behaves as before, except that messages marked
  silent no longer ring.

## Assumptions

- The bot is an administrator of the group, so it receives every command posted there; setting
  that up is the operator's job and outside tgshd.
- The operator's own user id is listed in `TELEGRAM_ALLOWED_IDS`.
- Topic ids are fixed for the life of a deployment; a changed id takes a restart.
- tgshd sends no other unprompted message besides those in the routing table; any message
  added later picks its row when it is added.
- Routing is fixed for tgshd's own messages; a per-check topic override is out of scope.
- The old channel keeps working until the operator switches `TELEGRAM_CHAT_ID`; after the switch
  it is no longer used by tgshd.

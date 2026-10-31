package route

// AccessList is the set of Telegram chat IDs allowed to command the bot.
type AccessList struct {
	ids map[int64]struct{}
}

// NewAccessList builds the allow-list from the configured chat ID and the
// optional extra IDs. A zero chatID (unset) is not added.
func NewAccessList(chatID int64, extra []int64) *AccessList {
	ids := make(map[int64]struct{}, len(extra)+1)
	if chatID != 0 {
		ids[chatID] = struct{}{}
	}
	for _, id := range extra {
		if id != 0 {
			ids[id] = struct{}{}
		}
	}

	return &AccessList{ids: ids}
}

// Empty reports whether no chat is allowed at all.
func (a *AccessList) Empty() bool {
	return len(a.ids) == 0
}

// Allowed reports whether updates from the given chat may be processed.
func (a *AccessList) Allowed(chatID int64) bool {
	_, ok := a.ids[chatID]
	return ok
}

// ChatKind is the kind of chat an update came from, as far as access goes.
type ChatKind int

const (
	// ChatChannel is a channel; only its posts reach the bot.
	ChatChannel ChatKind = iota + 1
	// ChatPrivate is a one-to-one chat with the bot.
	ChatPrivate
	// ChatGroup is a group or supergroup, where anyone can write.
	ChatGroup
)

// Permit reports whether a command from the given chat and sender may run.
// A channel or private chat needs the chat to be allowed. A group needs the
// chat and the sender to be allowed, so other members cannot command the bot.
func (a *AccessList) Permit(kind ChatKind, chatID, senderID int64) bool {
	if !a.Allowed(chatID) {
		return false
	}
	switch kind {
	case ChatChannel, ChatPrivate:
		return true
	case ChatGroup:
		return a.Allowed(senderID)
	default:
		return false
	}
}

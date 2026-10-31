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

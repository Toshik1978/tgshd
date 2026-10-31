package route

// Origin is where a command came from: the chat, the forum topic (0 for none)
// and the sender.
type Origin struct {
	ChatID   int64
	ThreadID int
	SenderID int64
}

// Reply is the destination for a direct reply to the command: same chat and
// topic, sent silently.
func (o Origin) Reply() Destination {
	return Destination{ChatID: o.ChatID, ThreadID: o.ThreadID, Silent: true}
}

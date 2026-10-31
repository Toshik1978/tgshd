package command

import (
	"context"

	"github.com/Toshik1978/tgshd/pkg/telegram/route"
)

// errPublisher is a Publisher fake that always returns a configurable error.
// It mirrors fakePublisher (defined in command_sms_test.go) but additionally
// supports simulating a publish failure, which fakePublisher does not.
type errPublisher struct {
	err      error
	count    int
	lastDest route.Destination
	lastMsg  string
}

func (f *errPublisher) Publish(_ context.Context, d route.Destination, msg string) error {
	f.count++
	f.lastDest = d
	f.lastMsg = msg
	return f.err
}

// testOrigin is a command that arrived in topic 3 of the given chat.
func testOrigin(chatID int64) route.Origin {
	return route.Origin{ChatID: chatID, ThreadID: 3, SenderID: 100}
}

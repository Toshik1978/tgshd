package command

import "context"

// errPublisher is a Publisher fake that always returns a configurable error.
// It mirrors fakePublisher (defined in command_sms_test.go) but additionally
// supports simulating a publish failure, which fakePublisher does not.
type errPublisher struct {
	err           error
	count         int
	lastRecipient int64
	lastMsg       string
}

func (f *errPublisher) Publish(_ context.Context, recipientID int64, msg string) error {
	f.count++
	f.lastRecipient = recipientID
	f.lastMsg = msg
	return f.err
}

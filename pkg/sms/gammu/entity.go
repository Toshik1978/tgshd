package gammu

// MessagePart declares one part of an SMS message.
type MessagePart struct {
	UDH    string
	Text   string
	Coding string
}

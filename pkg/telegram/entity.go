package telegram

// Document define telegram's document.
type Document struct {
	Name    string
	Content []byte
}

// Photo define telegram's photo.
type Photo struct {
	Caption string
	Content []byte
}

// Message define generic message.
type Message struct {
	Text     string
	Document *Document
	Photo    *Photo
}

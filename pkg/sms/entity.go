package sms

// Message declare SMS message.
type Message struct {
	Phone string `json:"phone"`
	Text  string `json:"text"`
}

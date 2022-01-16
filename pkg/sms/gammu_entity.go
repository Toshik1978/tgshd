package sms

// GammuOutbox declare outbox table.
type GammuOutbox struct {
	CreatorID         string `db:"CreatorID"`
	MultiPart         bool   `db:"MultiPart"`
	DestinationNumber string `db:"DestinationNumber"`
	UDH               string `db:"UDH"`
	TextDecoded       string `db:"TextDecoded"`
	Coding            string `db:"Coding"`
}

// GammuOutboxMultipart declare multipart outbox table.
type GammuOutboxMultipart struct {
	ID               int64  `db:"ID"`
	SequencePosition int    `db:"SequencePosition"`
	UDH              string `db:"UDH"`
	TextDecoded      string `db:"TextDecoded"`
	Coding           string `db:"Coding"`
}

func toModel(phone string, part gammuPart) GammuOutbox {
	return GammuOutbox{
		CreatorID:         sourceID,
		DestinationNumber: phone,
		TextDecoded:       part.Text,
		Coding:            part.Coding,
	}
}

func toModels(phone string, parts []gammuPart) (GammuOutbox, []GammuOutboxMultipart) {
	outbox := GammuOutbox{
		CreatorID:         sourceID,
		MultiPart:         true,
		DestinationNumber: phone,
		UDH:               parts[0].UDH,
		TextDecoded:       parts[0].Text,
		Coding:            parts[0].Coding,
	}
	outboxMultipart := make([]GammuOutboxMultipart, 0, len(parts)-1)
	for i := 1; i < len(parts); i++ {
		outboxMultipart = append(
			outboxMultipart,
			GammuOutboxMultipart{
				SequencePosition: i + 1,
				UDH:              parts[i].UDH,
				TextDecoded:      parts[i].Text,
				Coding:           parts[i].Coding,
			},
		)
	}
	return outbox, outboxMultipart
}

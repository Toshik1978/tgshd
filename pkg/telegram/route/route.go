// Package route holds the pure Telegram routing types: where a message goes
// (Destination), what a sender asks for (Route) and the forum-topic settings
// that turn the one into the other (Topics). It performs no I/O and imports
// nothing internal, so every sender can depend on it.
package route

import (
	"fmt"
	"strconv"
)

// Destination is where one Telegram message goes.
type Destination struct {
	ChatID int64
	// ThreadID is the forum topic id; 0 means no topic (General, or a chat
	// without topics).
	ThreadID int
	// Silent sends the message without notification sound.
	Silent bool
}

// Topic names one of tgshd's forum topics.
type Topic int

// The topics tgshd routes to. TopicNone is the zero value: no topic.
const (
	TopicNone Topic = iota
	TopicAlerts
	TopicSMS
	TopicSystem
)

// Route is what a sender asks for, before the chat is known.
type Route struct {
	Topic  Topic
	Silent bool
}

// String returns the persisted text form of r. A route outside the supported
// set has no text form and yields "".
func (r Route) String() string {
	switch r {
	case Route{Topic: TopicAlerts}:
		return "alerts"
	case Route{Topic: TopicAlerts, Silent: true}:
		return "alerts/silent"
	case Route{Topic: TopicSMS}:
		return "sms"
	case Route{Topic: TopicSystem, Silent: true}:
		return "system/silent"
	default:
		return ""
	}
}

// ParseRoute is the inverse of Route.String. Any text other than the
// supported forms is an error.
func ParseRoute(s string) (Route, error) {
	switch s {
	case "":
		return Route{}, nil
	case "alerts":
		return Route{Topic: TopicAlerts}, nil
	case "alerts/silent":
		return Route{Topic: TopicAlerts, Silent: true}, nil
	case "sms":
		return Route{Topic: TopicSMS}, nil
	case "system/silent":
		return Route{Topic: TopicSystem, Silent: true}, nil
	default:
		return Route{}, fmt.Errorf("unknown route %q", s)
	}
}

// Topics holds the forum thread id of each topic; 0 means unconfigured.
type Topics struct {
	Alerts int
	SMS    int
	System int
}

// ParseTopics parses the three TELEGRAM_TOPIC_* settings. A setting that is
// not a positive whole number, or that is empty while another one is set,
// resolves to thread 0 and yields one warning naming it. All three empty is
// valid and yields no warning.
func ParseTopics(alerts, sms, system string) (Topics, []string) {
	anySet := alerts != "" || sms != "" || system != ""
	var warnings []string
	parse := func(name, value string) int {
		if value == "" {
			if anySet {
				warnings = append(warnings, name+" is not set; its messages go without a topic")
			}
			return 0
		}
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			warnings = append(warnings, name+" is not a positive whole number; its messages go without a topic")
			return 0
		}

		return n
	}
	topics := Topics{
		Alerts: parse("TELEGRAM_TOPIC_ALERTS", alerts),
		SMS:    parse("TELEGRAM_TOPIC_SMS", sms),
		System: parse("TELEGRAM_TOPIC_SYSTEM", system),
	}

	return topics, warnings
}

// Resolve turns a route into a destination in the given chat. TopicNone and
// unconfigured topics resolve to thread 0.
func (t Topics) Resolve(chatID int64, r Route) Destination {
	d := Destination{ChatID: chatID, Silent: r.Silent}
	switch r.Topic {
	case TopicAlerts:
		d.ThreadID = t.Alerts
	case TopicSMS:
		d.ThreadID = t.SMS
	case TopicSystem:
		d.ThreadID = t.System
	case TopicNone:
	}

	return d
}

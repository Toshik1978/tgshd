package route

import (
	"strings"
	"testing"
)

func TestRouteRoundTrip(t *testing.T) {
	cases := []struct {
		text  string
		route Route
	}{
		{"", Route{}},
		{"alerts", Route{Topic: TopicAlerts}},
		{"alerts/silent", Route{Topic: TopicAlerts, Silent: true}},
		{"sms", Route{Topic: TopicSMS}},
		{"system/silent", Route{Topic: TopicSystem, Silent: true}},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			if got := tc.route.String(); got != tc.text {
				t.Fatalf("String() = %q, want %q", got, tc.text)
			}
			got, err := ParseRoute(tc.text)
			if err != nil {
				t.Fatalf("ParseRoute(%q): %v", tc.text, err)
			}
			if got != tc.route {
				t.Fatalf("ParseRoute(%q) = %+v, want %+v", tc.text, got, tc.route)
			}
		})
	}
}

func TestParseRouteRejectsUnknown(t *testing.T) {
	for _, in := range []string{"sms/silent", "system", "Alerts", "alerts/loud", " alerts", "x"} {
		if _, err := ParseRoute(in); err == nil {
			t.Errorf("ParseRoute(%q) succeeded, want error", in)
		}
	}
}

func TestParseTopics(t *testing.T) {
	cases := []struct {
		name                string
		alerts, sms, system string
		want                Topics
		warnFor             []string
	}{
		{"all empty", "", "", "", Topics{}, nil},
		{"all set", "2", "3", "4", Topics{Alerts: 2, SMS: 3, System: 4}, nil},
		{"one missing", "2", "", "4", Topics{Alerts: 2, System: 4}, []string{"TELEGRAM_TOPIC_SMS"}},
		{"only one set", "", "3", "", Topics{SMS: 3}, []string{"TELEGRAM_TOPIC_ALERTS", "TELEGRAM_TOPIC_SYSTEM"}},
		{"not a number", "abc", "3", "4", Topics{SMS: 3, System: 4}, []string{"TELEGRAM_TOPIC_ALERTS"}},
		{"zero", "2", "0", "4", Topics{Alerts: 2, System: 4}, []string{"TELEGRAM_TOPIC_SMS"}},
		{"negative", "2", "3", "-4", Topics{Alerts: 2, SMS: 3}, []string{"TELEGRAM_TOPIC_SYSTEM"}},
		{"fraction", "2.5", "3", "4", Topics{SMS: 3, System: 4}, []string{"TELEGRAM_TOPIC_ALERTS"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, warns := ParseTopics(tc.alerts, tc.sms, tc.system)
			if got != tc.want {
				t.Fatalf("topics = %+v, want %+v", got, tc.want)
			}
			if len(warns) != len(tc.warnFor) {
				t.Fatalf("warnings = %q, want %d", warns, len(tc.warnFor))
			}
			for i, name := range tc.warnFor {
				if !strings.Contains(warns[i], name) {
					t.Errorf("warning %q does not name %s", warns[i], name)
				}
			}
		})
	}
}

func TestTopicsResolve(t *testing.T) {
	const chat int64 = -1000000000000
	topics := Topics{Alerts: 2, SMS: 3, System: 4}
	cases := []struct {
		name  string
		route Route
		want  Destination
	}{
		{"zero route", Route{}, Destination{ChatID: chat}},
		{"alerts", Route{Topic: TopicAlerts}, Destination{ChatID: chat, ThreadID: 2}},
		{
			"alerts silent",
			Route{Topic: TopicAlerts, Silent: true},
			Destination{ChatID: chat, ThreadID: 2, Silent: true},
		},
		{"sms", Route{Topic: TopicSMS}, Destination{ChatID: chat, ThreadID: 3}},
		{
			"system silent",
			Route{Topic: TopicSystem, Silent: true},
			Destination{ChatID: chat, ThreadID: 4, Silent: true},
		},
		{"none silent", Route{Silent: true}, Destination{ChatID: chat, Silent: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := topics.Resolve(chat, tc.route); got != tc.want {
				t.Fatalf("Resolve = %+v, want %+v", got, tc.want)
			}
		})
	}
	if got := (Topics{}).Resolve(chat, Route{Topic: TopicSMS}); got.ThreadID != 0 {
		t.Fatalf("unconfigured topic ThreadID = %d, want 0", got.ThreadID)
	}
}

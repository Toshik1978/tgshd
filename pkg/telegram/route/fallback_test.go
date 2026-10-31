package route

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestThreadRejected(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"not found", errors.New("telegram: Bad Request: message thread not found (400)"), true},
		{"not found upper", errors.New("MESSAGE THREAD NOT FOUND"), true},
		{"closed", errors.New("telegram: TOPIC_CLOSED (400)"), true},
		{"closed lower", errors.New("topic_closed"), true},
		{"deleted", fmt.Errorf("wrapped: %w", errors.New("TOPIC_DELETED")), true},
		{"other", errors.New("chat not found"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ThreadRejected(tt.err); got != tt.want {
				t.Fatalf("ThreadRejected(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestSendWithFallback(t *testing.T) {
	rejected := errors.New("Bad Request: message thread not found")
	other := errors.New("chat not found")

	tests := []struct {
		name      string
		dest      Destination
		errs      []error // result of each send call
		wantSends []Destination
		wantWarns int
		wantErr   error
	}{
		{
			name:      "success",
			dest:      Destination{ChatID: -1000000000000, ThreadID: 2},
			errs:      []error{nil},
			wantSends: []Destination{{ChatID: -1000000000000, ThreadID: 2}},
		},
		{
			name: "rejected then retried without thread, silent kept",
			dest: Destination{ChatID: -1000000000000, ThreadID: 2, Silent: true},
			errs: []error{rejected, nil},
			wantSends: []Destination{
				{ChatID: -1000000000000, ThreadID: 2, Silent: true},
				{ChatID: -1000000000000, Silent: true},
			},
			wantWarns: 1,
		},
		{
			name:      "other error returned",
			dest:      Destination{ChatID: -1000000000000, ThreadID: 2},
			errs:      []error{other},
			wantSends: []Destination{{ChatID: -1000000000000, ThreadID: 2}},
			wantErr:   other,
		},
		{
			name:      "no thread never retried",
			dest:      Destination{ChatID: -1000000000000},
			errs:      []error{rejected},
			wantSends: []Destination{{ChatID: -1000000000000}},
			wantErr:   rejected,
		},
		{
			name:      "second failure returned",
			dest:      Destination{ChatID: -1000000000000, ThreadID: 2},
			errs:      []error{rejected, other},
			wantSends: []Destination{{ChatID: -1000000000000, ThreadID: 2}, {ChatID: -1000000000000}},
			wantWarns: 1,
			wantErr:   other,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sends []Destination
			var warnThreads []int
			send := func(_ context.Context, d Destination) error {
				i := len(sends)
				sends = append(sends, d)
				return tt.errs[i]
			}
			warn := func(threadID int, _ error) { warnThreads = append(warnThreads, threadID) }

			err := SendWithFallback(context.Background(), tt.dest, send, warn)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if len(sends) != len(tt.wantSends) {
				t.Fatalf("sends = %v, want %v", sends, tt.wantSends)
			}
			for i := range sends {
				if sends[i] != tt.wantSends[i] {
					t.Fatalf("send %d = %+v, want %+v", i, sends[i], tt.wantSends[i])
				}
			}
			if len(warnThreads) != tt.wantWarns {
				t.Fatalf("warns = %v, want %d", warnThreads, tt.wantWarns)
			}
			for _, id := range warnThreads {
				if id != tt.dest.ThreadID {
					t.Fatalf("warn thread = %d, want %d", id, tt.dest.ThreadID)
				}
			}
		})
	}
}

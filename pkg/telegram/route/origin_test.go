package route

import "testing"

func TestOriginReply(t *testing.T) {
	tests := []struct {
		name string
		in   Origin
		want Destination
	}{
		{
			"topic",
			Origin{ChatID: -1000000000000, ThreadID: 2, SenderID: 100},
			Destination{ChatID: -1000000000000, ThreadID: 2, Silent: true},
		},
		{"no topic", Origin{ChatID: 100, SenderID: 100}, Destination{ChatID: 100, Silent: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.Reply(); got != tt.want {
				t.Fatalf("Reply() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

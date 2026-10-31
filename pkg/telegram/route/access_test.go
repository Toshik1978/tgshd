package route

import "testing"

func TestAccessListAllowed(t *testing.T) {
	tests := []struct {
		name   string
		chatID int64
		extra  []int64
		query  int64
		want   bool
	}{
		{"configured chat", 100, nil, 100, true},
		{"extra id", 100, []int64{200, -300}, -300, true},
		{"stranger", 100, []int64{200}, 999, false},
		{"unset chat does not allow zero", 0, nil, 0, false},
		{"unset chat with extras", 0, []int64{200}, 200, true},
		{"empty list denies all", 0, nil, 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NewAccessList(tt.chatID, tt.extra).Allowed(tt.query); got != tt.want {
				t.Fatalf("Allowed(%d) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestAccessListEmpty(t *testing.T) {
	if !NewAccessList(0, nil).Empty() {
		t.Fatal("list with no IDs must be empty")
	}

	if NewAccessList(1, nil).Empty() {
		t.Fatal("list with an ID must not be empty")
	}
}

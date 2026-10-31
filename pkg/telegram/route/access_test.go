package route

import (
	"slices"
	"testing"
)

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

func TestAccessListPermit(t *testing.T) {
	const group = int64(-1000000000000)
	access := NewAccessList(100, []int64{group, 200})

	tests := []struct {
		name     string
		kind     ChatKind
		chatID   int64
		senderID int64
		want     bool
	}{
		{"channel allowed", ChatChannel, group, 0, true},
		{"channel not allowed", ChatChannel, -999, 0, false},
		{"private allowed", ChatPrivate, 100, 100, true},
		{"private not allowed", ChatPrivate, 999, 999, false},
		{"group with allowed sender", ChatGroup, group, 200, true},
		{"group with stranger", ChatGroup, group, 999, false},
		{"group without sender", ChatGroup, group, 0, false},
		{"group not allowed, sender allowed", ChatGroup, -999, 200, false},
		{"unknown kind", ChatKind(99), group, 200, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := access.Permit(tt.kind, tt.chatID, tt.senderID); got != tt.want {
				t.Fatalf("Permit(%v, %d, %d) = %v, want %v", tt.kind, tt.chatID, tt.senderID, got, tt.want)
			}
		})
	}
}

func TestAccessListMenuScopes(t *testing.T) {
	const (
		userA  = int64(100000000)
		userB  = int64(200000000)
		groupA = int64(-1000000000000)
		groupB = int64(-1000000000001)
	)

	tests := []struct {
		name string
		ids  []int64
		want []MenuScope
	}{
		{name: "empty list", ids: nil, want: nil},
		{
			name: "users only",
			ids:  []int64{userB, userA},
			want: []MenuScope{
				{Kind: ScopePrivate, ChatID: userA},
				{Kind: ScopePrivate, ChatID: userB},
			},
		},
		{name: "groups without users", ids: []int64{groupA, groupB}, want: nil},
		{
			name: "groups and users",
			ids:  []int64{groupB, userB, groupA, userA},
			want: []MenuScope{
				{Kind: ScopePrivate, ChatID: userA},
				{Kind: ScopePrivate, ChatID: userB},
				{Kind: ScopeGroupMember, ChatID: groupB, UserID: userA},
				{Kind: ScopeGroupMember, ChatID: groupB, UserID: userB},
				{Kind: ScopeGroupMember, ChatID: groupA, UserID: userA},
				{Kind: ScopeGroupMember, ChatID: groupA, UserID: userB},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewAccessList(0, tt.ids).MenuScopes()
			if !slices.Equal(got, tt.want) {
				t.Fatalf("MenuScopes() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestScopeKindZeroIsInvalid(t *testing.T) {
	if ScopePrivate == 0 || ScopeGroupMember == 0 {
		t.Fatal("zero ScopeKind must not be a valid kind")
	}
}

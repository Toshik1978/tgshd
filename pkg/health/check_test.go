package health

import (
	"errors"
	"fmt"
	"testing"
)

func TestCheckErrorKey(t *testing.T) {
	tests := []struct {
		name  string
		err   CheckError
		check string
		want  string
	}{
		{
			"subject and kind",
			CheckError{Subject: "TEST0000000001", Kind: "no-verdict"},
			"disks",
			"disks/TEST0000000001/no-verdict",
		},
		{"empty subject", CheckError{Kind: "auth"}, "http/example", "http/example/auth"},
		{"fixed component", CheckError{Subject: "kmsg", Kind: "unreadable"}, "kernel", "kernel/kmsg/unreadable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Key(tt.check); got != tt.want {
				t.Fatalf("Key(%q) = %q, want %q", tt.check, got, tt.want)
			}
		})
	}
}

func TestCheckErrorErrorAndUnwrap(t *testing.T) {
	cause := errors.New("boom")
	ce := CheckError{Subject: "TEST0000000001", Kind: "no-verdict", Err: cause}
	if !errors.Is(ce, cause) {
		t.Fatal("errors.Is does not reach the cause")
	}
	if got, want := ce.Error(), "TEST0000000001/no-verdict: boom"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if got, want := (CheckError{Kind: "auth", Err: cause}).Error(), "auth: boom"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if got, want := (CheckError{Subject: "cpu", Kind: "lost"}).Error(), "cpu/lost"; got != want {
		t.Fatalf("Error() nil cause = %q, want %q", got, want)
	}
}

func TestCheckErrors(t *testing.T) {
	a := CheckError{Subject: "a", Kind: "lost", Err: errors.New("x")}
	b := CheckError{Subject: "b", Kind: "incomplete"}
	plain := errors.New("plain")
	tests := []struct {
		name string
		err  error
		want []string
	}{
		{"nil", nil, nil},
		{"plain", plain, nil},
		{"single", a, []string{"a/lost"}},
		{"joined", errors.Join(a, plain, b), []string{"a/lost", "b/incomplete"}},
		{
			"wrapped joined",
			fmt.Errorf("run: %w", errors.Join(plain, errors.Join(a, b))),
			[]string{"a/lost", "b/incomplete"},
		},
		{"multi-%w", fmt.Errorf("%w and %w", a, b), []string{"a/lost", "b/incomplete"}},
		{"nested check error not descended", CheckError{Subject: "o", Kind: "lost", Err: a}, []string{"o/lost"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckErrors(tt.err)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d errors %v, want %v", len(got), got, tt.want)
			}
			for i, w := range tt.want {
				if k := got[i].Key("c"); k != "c/"+w {
					t.Fatalf("[%d] key = %q, want %q", i, k, w)
				}
			}
		})
	}
}

func TestSplitErrors(t *testing.T) {
	a := CheckError{Subject: "a", Kind: "lost"}
	b := CheckError{Subject: "b", Kind: "incomplete"}
	p1, p2 := errors.New("p1"), errors.New("p2")
	tests := []struct {
		name   string
		err    error
		checks int
		plain  []string
	}{
		{"nil", nil, 0, nil},
		{"plain", p1, 0, []string{"p1"}},
		{"check only", a, 1, nil},
		{"flat join", errors.Join(a, p1, b), 2, []string{"p1"}},
		{"nested join", errors.Join(errors.Join(a, p1), b), 2, []string{"p1"}},
		{"wrapped join", fmt.Errorf("scan: %w", errors.Join(a, p1)), 1, []string{"p1"}},
		{"several plain", errors.Join(p1, a, p2), 1, []string{"p1", "p2"}},
		{"plain subtree kept whole", errors.Join(a, fmt.Errorf("ctx: %w", p1)), 1, []string{"ctx: p1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checks, plain := SplitErrors(tt.err)
			if len(checks) != tt.checks {
				t.Fatalf("checks = %v, want %d", checks, tt.checks)
			}
			if len(plain) != len(tt.plain) {
				t.Fatalf("plain = %v, want %v", plain, tt.plain)
			}
			for i, w := range tt.plain {
				if plain[i].Error() != w {
					t.Fatalf("plain[%d] = %q, want %q", i, plain[i], w)
				}
			}
		})
	}
}

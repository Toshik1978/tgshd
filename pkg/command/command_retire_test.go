package command

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/health/runner"
)

type fakeRetirer struct {
	res  runner.RetireResult
	err  error
	args []string
}

func (f *fakeRetirer) Retire(_ context.Context, serial string) (runner.RetireResult, error) {
	f.args = append(f.args, serial)

	return f.res, f.err
}

func TestRetireCommand(t *testing.T) {
	d := runner.Drive{Serial: "TEST0000000001", Model: "Example <SSD>", Bus: "nvme"}
	known := []runner.Drive{d, {Serial: "TEST0000000002", Model: "Example HDD", Bus: "ata"}}
	cases := []struct {
		name    string
		cmd     string
		res     runner.RetireResult
		err     error
		wantArg string
		want    []string
		absent  []string
	}{
		{
			"retired", "/retire 0001",
			runner.RetireResult{Drive: d, Cleared: 3},
			nil, "0001",
			[]string{"Retired nvme Example &lt;SSD&gt; …0001; cleared 3 conditions."},
			[]string{"TEST0000000001"},
		},
		{
			"ambiguous", "/retire 0000",
			runner.RetireResult{Candidates: known},
			runner.ErrAmbiguousDrive, "0000",
			[]string{"Ambiguous", "…0001", "…0002"},
			[]string{"TEST0000"},
		},
		{
			"unknown", "/retire ZZZZ",
			runner.RetireResult{Candidates: known},
			runner.ErrNoDrive, "ZZZZ",
			[]string{"Unknown drive", "…0001", "…0002"},
			nil,
		},
		{
			"usage", "/retire",
			runner.RetireResult{Candidates: known},
			runner.ErrNoDrive, "",
			[]string{"Usage: /retire", "…0001"},
			nil,
		},
		{
			"none known", "/retire",
			runner.RetireResult{},
			runner.ErrNoDrive, "",
			[]string{"No known drives"},
			nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pub := &fakePublisher{}
			r := &fakeRetirer{res: c.res, err: c.err}
			cmd := NewRetireCommand(zap.NewNop(), pub, r)
			if cmd.Name() != "retire" || !cmd.Enabled() {
				t.Fatalf("Name=%q Enabled=%v", cmd.Name(), cmd.Enabled())
			}
			if err := cmd.Handle(context.Background(), testOrigin(9), c.cmd); err != nil {
				t.Fatal(err)
			}
			if len(r.args) != 1 || r.args[0] != c.wantArg {
				t.Errorf("args = %v, want %q", r.args, c.wantArg)
			}
			for _, w := range c.want {
				if !strings.Contains(pub.lastMsg, w) {
					t.Errorf("missing %q in %q", w, pub.lastMsg)
				}
			}
			for _, a := range c.absent {
				if strings.Contains(pub.lastMsg, a) {
					t.Errorf("unexpected %q in %q", a, pub.lastMsg)
				}
			}
		})
	}
}

func TestRetireCommandErrors(t *testing.T) {
	boom := errors.New("boom")
	failing := NewRetireCommand(zap.NewNop(), &fakePublisher{}, &fakeRetirer{err: boom})
	if err := failing.Handle(context.Background(), testOrigin(1), "/retire x"); !errors.Is(err, boom) {
		t.Errorf("retire err = %v", err)
	}
	pub := &errPublisher{err: boom}
	cmd := NewRetireCommand(zap.NewNop(), pub, &fakeRetirer{err: runner.ErrNoDrive})
	if err := cmd.Handle(context.Background(), testOrigin(1), "/retire"); !errors.Is(err, boom) {
		t.Errorf("publish err = %v", err)
	}
}

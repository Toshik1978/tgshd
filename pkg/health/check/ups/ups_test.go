package ups

import (
	"context"
	"errors"
	"testing"

	"github.com/Toshik1978/tgshd/pkg/health"
)

type fakeSource struct {
	status Status
	err    error
}

func (f fakeSource) Status(context.Context) (Status, error) { return f.status, f.err }

func TestRunGood(t *testing.T) {
	res := New(
		fakeSource{status: Status{State: "OL", Charge: new(float64(100)), Voltage: new(float64(230))}},
		"main",
	).Run(context.Background())

	if res.Err != nil || len(res.Readings) != 1 {
		t.Fatalf("unexpected result %+v", res)
	}
	r := res.Readings[0]
	if r.Key != "ups/main" || r.Verdict != health.Good || r.Kind != health.State {
		t.Fatalf("unexpected reading %+v", r)
	}
	want := []health.Sample{{Metric: "ups.charge", Value: 100}, {Metric: "ups.voltage", Value: 230}}
	if len(res.Samples) != 2 || res.Samples[0] != want[0] || res.Samples[1] != want[1] {
		t.Fatalf("samples %+v", res.Samples)
	}
}

func TestRunPartialSamples(t *testing.T) {
	res := New(fakeSource{status: Status{State: "OB", Charge: new(float64(50))}}, "").Run(context.Background())

	if res.Readings[0].Key != "ups/nut" || len(res.Samples) != 1 || res.Samples[0].Metric != "ups.charge" {
		t.Fatalf("unexpected result %+v", res)
	}
}

func TestRunBad(t *testing.T) {
	for _, err := range []error{
		errors.New("ups data is stale"),
		errors.New("driver not connected"),
		errors.New("connection failed"),
		errors.New("empty"),
		context.DeadlineExceeded,
	} {
		res := New(fakeSource{err: err}, "main").Run(context.Background())

		if res.Err != nil || len(res.Readings) != 1 || len(res.Samples) != 0 {
			t.Fatalf("%v: unexpected result %+v", err, res)
		}
		r := res.Readings[0]
		if r.Key != "ups/main" || r.Verdict != health.Bad || r.Text == "" {
			t.Fatalf("%v: unexpected reading %+v", err, r)
		}
	}
}

func TestRunCancelledIsCheckError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := New(fakeSource{err: context.Canceled}, "main").Run(ctx)
	if !errors.Is(res.Err, context.Canceled) || len(res.Readings) != 0 {
		t.Fatalf("unexpected result %+v", res)
	}
}

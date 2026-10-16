package power

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	nut "github.com/robbiet480/go.nut"
)

func TestMapNUTError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"stale", errors.New("ERR DATA-STALE"), ErrStale},
		{"driver", errors.New("failed: ERR DRIVER-NOT-CONNECTED"), ErrDriverNotConnected},
		{"other", errors.New("ERR UNKNOWN-UPS"), ErrConnection},
		{"io", errors.New("read tcp: broken pipe"), ErrConnection},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mapNUTError(tt.err); !errors.Is(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildStatus(t *testing.T) {
	st, err := buildStatus([]nut.Variable{
		{Name: "ups.status", Value: "OL"},
		{Name: "battery.charge", Value: int64(100)},
		{Name: "input.voltage", Value: 229.5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "OL" || st.Charge == nil || *st.Charge != 100 || st.Voltage == nil || *st.Voltage != 229.5 {
		t.Fatalf("unexpected status %+v", st)
	}

	st, err = buildStatus([]nut.Variable{{Name: "ups.status", Value: "OB"}})
	if err != nil || st.Charge != nil || st.Voltage != nil {
		t.Fatalf("unexpected %+v %v", st, err)
	}

	if _, err = buildStatus(nil); !errors.Is(err, ErrEmpty) {
		t.Fatalf("got %v, want ErrEmpty", err)
	}
	if _, err = buildStatus([]nut.Variable{{Name: "device.model", Value: "x"}}); !errors.Is(err, ErrEmpty) {
		t.Fatalf("got %v, want ErrEmpty", err)
	}
}

func TestStatusTimeout(t *testing.T) {
	p := &power{reader: func() (Status, error) {
		time.Sleep(time.Second)

		return Status{}, nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if _, err := p.Status(ctx); !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v, want ErrTimeout", err)
	}
}

func TestStatusPassesThrough(t *testing.T) {
	p := &power{reader: func() (Status, error) { return Status{State: "OL"}, nil }}
	st, err := p.Status(context.Background())
	if err != nil || st.State != "OL" {
		t.Fatalf("got %+v %v", st, err)
	}
}

func TestStatusSingleFlight(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	p := &power{reader: func() (Status, error) {
		calls.Add(1)
		<-release

		return Status{}, nil
	}}

	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, err := p.Status(ctx)
		cancel()
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("got %v, want ErrTimeout", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("reader spawned %d times, want 1", got)
	}

	close(release)
	deadline := time.Now().Add(time.Second)
	for p.inflight.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if _, err := p.Status(context.Background()); err != nil {
		t.Fatalf("after release: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("reader calls %d, want 2", got)
	}
}

func TestStatusCancelledIsNotTimeout(t *testing.T) {
	p := &power{reader: func() (Status, error) {
		time.Sleep(time.Second)

		return Status{}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := p.Status(ctx); !errors.Is(err, context.Canceled) || errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v, want plain context.Canceled", err)
	}
}

package docker

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

type fakeEngine struct {
	listed     []container.Summary
	inspects   map[string]container.InspectResponse
	inspectErr map[string]error
	idFilter   map[string]bool // ids still present on a filtered list
	listErr    error
	filterErr  error
	nameMatch  []container.Summary // returned by a list filtered by name
	nameErr    error
	stopped    []string
	stopErr    error
	inspected  []string
	deadlines  []time.Time // the deadline of each restart and stop call
}

func (f *fakeEngine) ContainerList(
	_ context.Context, o client.ContainerListOptions,
) (client.ContainerListResult, error) {
	if _, ok := o.Filters["name"]; ok {
		return client.ContainerListResult{Items: f.nameMatch}, f.nameErr
	}
	if ids, ok := o.Filters["id"]; ok {
		if f.filterErr != nil {
			return client.ContainerListResult{}, f.filterErr
		}
		var items []container.Summary
		for id := range ids {
			if f.idFilter[id] {
				items = append(items, container.Summary{ID: id})
			}
		}

		return client.ContainerListResult{Items: items}, nil
	}
	if f.listErr != nil {
		return client.ContainerListResult{}, f.listErr
	}

	return client.ContainerListResult{Items: f.listed}, nil
}

func (f *fakeEngine) ContainerInspect(
	_ context.Context, id string, _ client.ContainerInspectOptions,
) (client.ContainerInspectResult, error) {
	f.inspected = append(f.inspected, id)
	if err := f.inspectErr[id]; err != nil {
		return client.ContainerInspectResult{}, err
	}

	return client.ContainerInspectResult{Container: f.inspects[id]}, nil
}

func (f *fakeEngine) ContainerRestart(
	ctx context.Context, _ string, _ client.ContainerRestartOptions,
) (client.ContainerRestartResult, error) {
	f.recordDeadline(ctx)

	return client.ContainerRestartResult{}, nil
}

func (f *fakeEngine) recordDeadline(ctx context.Context) {
	d, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, d)
}

func (f *fakeEngine) ContainerStop(
	ctx context.Context, id string, _ client.ContainerStopOptions,
) (client.ContainerStopResult, error) {
	f.recordDeadline(ctx)
	f.stopped = append(f.stopped, id)

	return client.ContainerStopResult{}, f.stopErr
}

func (*fakeEngine) Close() error { return nil }

func TestList(t *testing.T) {
	errBoom := errors.New("boom")
	base := func() *fakeEngine {
		return &fakeEngine{
			listed: []container.Summary{{ID: "a"}, {ID: "b"}},
			inspects: map[string]container.InspectResponse{
				"a": {Name: "/a"}, "b": {Name: "/b"},
			},
			inspectErr: map[string]error{},
			idFilter:   map[string]bool{},
		}
	}

	tests := []struct {
		name    string
		mutate  func(*fakeEngine)
		want    []string
		wantErr bool
	}{
		{name: "all inspected", mutate: func(*fakeEngine) {}, want: []string{"a", "b"}},
		{
			name:   "vanished container skipped",
			mutate: func(f *fakeEngine) { f.inspectErr["a"] = errBoom },
			want:   []string{"b"},
		},
		{
			name: "inspect error and still listed",
			mutate: func(f *fakeEngine) {
				f.inspectErr["a"] = errBoom
				f.idFilter["a"] = true
			},
			wantErr: true,
		},
		{
			name: "inspect error and relist error",
			mutate: func(f *fakeEngine) {
				f.inspectErr["a"] = errBoom
				f.filterErr = errBoom
			},
			wantErr: true,
		},
		{name: "list error", mutate: func(f *fakeEngine) { f.listErr = errBoom }, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := base()
			tt.mutate(f)

			got, err := (&Client{api: f}).List(t.Context())
			if (err != nil) != tt.wantErr {
				t.Fatalf("List() error = %v, wantErr %v", err, tt.wantErr)
			}

			names := make([]string, 0, len(got))
			for _, c := range got {
				names = append(names, c.Name)
			}
			if !slices.Equal(names, tt.want) {
				t.Errorf("List() names = %v, want %v", names, tt.want)
			}
		})
	}
}

func TestInspect(t *testing.T) {
	errBoom := errors.New("boom")
	f := &fakeEngine{
		inspects: map[string]container.InspectResponse{
			"web": {Name: "/web", State: &container.State{Running: true, StartedAt: "2026-01-02T03:04:05Z"}},
		},
		inspectErr: map[string]error{"gone": errBoom},
	}
	c := &Client{api: f}

	got, err := c.Inspect(t.Context(), "web")
	if err != nil || got.Name != "web" || !got.Running || got.StartedAt.IsZero() {
		t.Fatalf("Inspect(web) = %+v, %v", got, err)
	}
	if _, err := c.Inspect(t.Context(), "gone"); !errors.Is(err, errBoom) {
		t.Fatalf("Inspect(gone) error = %v, want the engine error", err)
	}
}

func TestInspectNotFound(t *testing.T) {
	errBoom := errors.New("boom")

	tests := []struct {
		name         string
		nameMatch    []container.Summary
		nameErr      error
		wantNotFound bool
	}{
		{name: "re-list finds nothing", wantNotFound: true},
		{
			name:         "re-list finds only a similar name",
			nameMatch:    []container.Summary{{Names: []string{"/gone-2"}}},
			wantNotFound: true,
		},
		{name: "re-list finds the container", nameMatch: []container.Summary{{Names: []string{"/gone"}}}},
		{name: "re-list fails", nameErr: errBoom},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{
				inspectErr: map[string]error{"gone": errBoom},
				nameMatch:  tt.nameMatch,
				nameErr:    tt.nameErr,
			}

			_, err := (&Client{api: f}).Inspect(t.Context(), "gone")
			if err == nil {
				t.Fatal("Inspect() error = nil, want an error")
			}
			if got := errors.Is(err, ErrNotFound); got != tt.wantNotFound {
				t.Errorf("errors.Is(err, ErrNotFound) = %v, want %v (err = %v)", got, tt.wantNotFound, err)
			}
			if !errors.Is(err, errBoom) {
				t.Errorf("the engine error is not wrapped: %v", err)
			}
		})
	}
}

func TestStop(t *testing.T) {
	errBoom := errors.New("boom")

	f := &fakeEngine{}
	if err := (&Client{api: f}).Stop(context.Background(), "web-app"); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if !slices.Equal(f.stopped, []string{"web-app"}) {
		t.Errorf("stopped = %v, want [web-app]", f.stopped)
	}

	f = &fakeEngine{stopErr: errBoom}
	err := (&Client{api: f}).Stop(context.Background(), "web-app")
	if !errors.Is(err, errBoom) {
		t.Errorf("Stop() error = %v, want wrapping %v", err, errBoom)
	}
}

// TestActionsKeepTheCallersDeadline pins that a restart or a stop is bounded
// by the caller's deadline, however far it is, and only falls back to the
// adapter's own bound when the caller set none.
func TestActionsKeepTheCallersDeadline(t *testing.T) {
	f := &fakeEngine{}
	c := &Client{api: f}
	far := time.Now().Add(5 * time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), far)
	defer cancel()

	if err := c.Restart(ctx, "web-app"); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(ctx, "web-app"); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if err := c.Restart(context.Background(), "web-app"); err != nil {
		t.Fatal(err)
	}
	if len(f.deadlines) != 3 {
		t.Fatalf("deadlines = %v, want 3", f.deadlines)
	}
	for i, d := range f.deadlines[:2] {
		if !d.Equal(far) {
			t.Errorf("call %d: deadline = %v, want the caller's %v", i, d, far)
		}
	}
	if d := f.deadlines[2]; d.IsZero() || d.Before(before.Add(2*callTimeout-time.Second)) ||
		d.After(time.Now().Add(2*callTimeout)) {
		t.Errorf("no caller deadline: deadline = %v, want about %v from now", d, 2*callTimeout)
	}
}

// TestNames maps ids to names from the list alone, with no inspect.
func TestNames(t *testing.T) {
	errBoom := errors.New("boom")
	f := &fakeEngine{listed: []container.Summary{
		{ID: "aaa", Names: []string{"/web-app"}},
		{ID: "bbb", Names: []string{"/db", "/other/db"}},
		{ID: "ccc"},
	}}
	got, err := (&Client{api: f}).Names(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"aaa": "web-app", "bbb": "db"}; !maps.Equal(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	if len(f.inspected) != 0 {
		t.Fatalf("inspected %v, want no inspect", f.inspected)
	}

	f.listErr = errBoom
	if _, err := (&Client{api: f}).Names(context.Background()); !errors.Is(err, errBoom) {
		t.Fatalf("Names() error = %v, want wrapping %v", err, errBoom)
	}
}

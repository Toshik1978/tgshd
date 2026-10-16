package docker

import (
	"context"
	"errors"
	"slices"
	"testing"

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
	if err := f.inspectErr[id]; err != nil {
		return client.ContainerInspectResult{}, err
	}

	return client.ContainerInspectResult{Container: f.inspects[id]}, nil
}

func (*fakeEngine) ContainerRestart(
	context.Context, string, client.ContainerRestartOptions,
) (client.ContainerRestartResult, error) {
	return client.ContainerRestartResult{}, nil
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

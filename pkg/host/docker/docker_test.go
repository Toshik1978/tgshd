package docker

import (
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
)

func TestMapContainer(t *testing.T) {
	started := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)

	tests := []struct {
		name string
		in   container.InspectResponse
		want Container
	}{
		{
			name: "running healthy",
			in: container.InspectResponse{
				Name:         "/web",
				RestartCount: 3,
				Path:         "/secret-binary",
				Args:         []string{"--token=hunter2"},
				Config:       &container.Config{Env: []string{"PASSWORD=x"}, Labels: map[string]string{"a": "b"}},
				HostConfig: &container.HostConfig{
					RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyAlways},
				},
				State: &container.State{
					Status: container.StateRunning, Running: true,
					StartedAt: started.Format(time.RFC3339Nano),
					Health:    &container.Health{Status: container.Healthy},
				},
			},
			want: Container{
				Name: "web", RestartPolicy: "always", Running: true, Status: "running",
				Health: "healthy", RestartCount: 3, StartedAt: started,
			},
		},
		{
			name: "nil sections and bad time",
			in: container.InspectResponse{
				Name:  "db",
				State: &container.State{Status: container.StateExited, StartedAt: "garbage"},
			},
			want: Container{Name: "db", Status: "exited"},
		},
		{
			name: "everything nil",
			in:   container.InspectResponse{Name: "/x"},
			want: Container{Name: "x"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapContainer(tt.in)
			if got != tt.want {
				t.Errorf("mapContainer() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestNewDoesNotRequireDocker(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/tgshd-test.sock")

	c, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer c.Close()

	if _, err := c.List(t.Context()); err == nil {
		t.Error("List() with a missing socket: want error, got nil")
	}
	if _, err := c.Inspect(t.Context(), "web"); err == nil {
		t.Error("Inspect() with a missing socket: want error, got nil")
	}
	if err := c.Restart(t.Context(), "web"); err == nil {
		t.Error("Restart() with a missing socket: want error, got nil")
	}
}

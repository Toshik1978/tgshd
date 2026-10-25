package memguard

import (
	"errors"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
)

const gb = uint64(1) << 30

func TestFormatSize(t *testing.T) {
	tests := []struct {
		in   uint64
		want string
	}{
		{0, "0.0 GB"},
		{gb * 56 / 10, "5.6 GB"},
		{9*gb + gb*96/100, "10 GB"},
		{31 * gb, "31 GB"},
		{62 * gb, "62 GB"},
		{gb / 2, "0.5 GB"},
	}
	for _, tt := range tests {
		if got := formatSize(tt.in); got != tt.want {
			t.Errorf("formatSize(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFormatSpan(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{10 * time.Minute, "10m"},
		{2*time.Hour + 10*time.Minute, "2h10m"},
		{2 * time.Hour, "2h"},
		{20 * time.Second, "1m"},
	}
	for _, tt := range tests {
		if got := formatSpan(tt.in); got != tt.want {
			t.Errorf("formatSpan(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func culpritOf(kind Kind, name string, before, size uint64) Culprit {
	id := "container:" + name
	if kind == KindUnit {
		id = "unit:" + name
	}

	return Culprit{
		Service: Service{ID: id, Kind: kind, Name: name},
		Before:  before, Size: size, Growth: size - before, Window: 10 * time.Minute,
	}
}

func TestMessage(t *testing.T) {
	cfg := Config{Channel: health.ModeTelegram, FailedChannel: health.ModeSMS}
	web := culpritOf(KindContainer, "web-app", 12*gb, 31*gb)
	worker := Top{Name: "worker", Size: 20 * gb}
	tests := []struct {
		name string
		out  Outcome
		key  string
		mode health.Mode
		text string
	}{
		{
			"restarted",
			Outcome{Kind: OutcomeRestarted, Culprit: web, AvailablePct: 4, FullAvg10: 35},
			"memory/guard/web-app", health.ModeTelegram,
			"memory: restarted container web-app (grew 12→31 GB in 10 min); host 4% available, stalled 35%",
		},
		{
			"would restart",
			Outcome{Kind: OutcomeWouldRestart, Culprit: web, AvailablePct: 4, FullAvg10: 35},
			"memory/guard/web-app",
			health.ModeTelegram,
			"memory: would restart container web-app (grew 12→31 GB in 10 min); host 4% available, stalled 35% — intervene is report",
		},
		{
			"stopped",
			Outcome{
				Kind:         OutcomeStopped,
				Culprit:      culpritOf(KindContainer, "web-app", 3*gb, 22*gb),
				AvailablePct: 4,
				At: time.Date(
					2026,
					1,
					1,
					12,
					10,
					0,
					0,
					time.UTC,
				),
				LastAction: time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC),
			},
			"memory/guard/web-app",
			health.ModeSMS,
			"memory: stopped container web-app: ran away again 2h10m after tgshd restarted it (grew 3→22 GB in 10 min); host 4% available",
		},
		{
			"would stop",
			Outcome{
				Kind:         OutcomeWouldStop,
				Culprit:      culpritOf(KindContainer, "web-app", 3*gb, 22*gb),
				AvailablePct: 4,
				FullAvg10:    35,
				At: time.Date(
					2026,
					1,
					1,
					12,
					10,
					0,
					0,
					time.UTC,
				),
				LastAction: time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC),
			},
			"memory/guard/web-app",
			health.ModeTelegram,
			"memory: would stop container web-app: ran away again 2h10m after tgshd restarted it (grew 3→22 GB in 10 min); host 4% available, stalled 35% — intervene is report",
		},
		{
			"killed unit",
			Outcome{
				Kind: OutcomeKilled, Culprit: culpritOf(KindUnit, "backup.service", 2*gb, 12*gb), AvailablePct: 2,
				Timeout: 60 * time.Second,
			},
			"memory/guard/backup.service",
			health.ModeSMS,
			"memory: killed service backup.service through the kernel: systemctl did not answer within 60s (grew 2→12 GB in 10 min); host 2% available",
		},
		{
			"killed container",
			Outcome{
				Kind: OutcomeKilled, Culprit: web, AvailablePct: 2, Timeout: 60 * time.Second,
			},
			"memory/guard/web-app",
			health.ModeSMS,
			"memory: killed container web-app through the kernel: docker did not answer within 60s (grew 12→31 GB in 10 min); host 2% available",
		},
		{
			"killed unit, runtime unreachable",
			Outcome{
				Kind: OutcomeKilled, Culprit: culpritOf(KindUnit, "backup.service", 2*gb, 12*gb), AvailablePct: 2,
				Unreachable: true,
			},
			"memory/guard/backup.service",
			health.ModeSMS,
			"memory: killed service backup.service through the kernel: systemctl could not be reached (grew 2→12 GB in 10 min); host 2% available",
		},
		{
			"killed container, runtime unreachable",
			Outcome{
				Kind: OutcomeKilled, Culprit: web, AvailablePct: 2, Unreachable: true,
			},
			"memory/guard/web-app",
			health.ModeSMS,
			"memory: killed container web-app through the kernel: docker could not be reached (grew 12→31 GB in 10 min); host 2% available",
		},
		{
			"not recovered",
			Outcome{
				Kind:         OutcomeNotRecovered,
				Action:       ActionRestart,
				Culprit:      web,
				AvailablePct: 3,
				FullAvg10:    45,
				Top:          worker,
			},
			"memory/guard/web-app",
			health.ModeSMS,
			"memory: restarting web-app (31 GB before) did not free memory; host still 3% available, stalled 45%; top: worker 20 GB",
		},
		{
			"not recovered without top",
			Outcome{Kind: OutcomeNotRecovered, Action: ActionStop, Culprit: web, AvailablePct: 3, FullAvg10: 45},
			"memory/guard/web-app", health.ModeSMS,
			"memory: stopping web-app (31 GB before) did not free memory; host still 3% available, stalled 45%",
		},
		{
			"protected",
			Outcome{
				Kind:         OutcomeProtected,
				Culprit:      culpritOf(KindContainer, "db", 12*gb, 40*gb),
				AvailablePct: 3,
				FullAvg10:    45,
			},
			"memory/guard/db",
			health.ModeSMS,
			"memory: host 3% available, stalled 45%; db is growing (12→40 GB in 10 min) but is never-touch, tgshd did not act",
		},
		{
			"no culprit",
			Outcome{
				Kind: OutcomeNoCulprit, AvailablePct: 3, FullAvg10: 45,
				Top: Top{Name: "python3", PID: 4242, Size: 40 * gb, Slice: "user.slice"},
			},
			"memory/guard/host",
			health.ModeSMS,
			"memory: host 3% available, stalled 45%; no service is growing; top process: python3 (pid 4242) 40 GB in user.slice",
		},
		{
			"no culprit without process",
			Outcome{Kind: OutcomeNoCulprit, AvailablePct: 3, FullAvg10: 45},
			"memory/guard/host", health.ModeSMS,
			"memory: host 3% available, stalled 45%; no service is growing",
		},
		{
			"action failed",
			Outcome{
				Kind:         OutcomeActionFailed,
				Action:       ActionRestart,
				Culprit:      web,
				AvailablePct: 3,
				Err:          errors.New("docker: boom"),
			},
			"memory/guard/web-app",
			health.ModeSMS,
			"memory: could not restart web-app (31 GB): docker: boom; host 3% available",
		},
		{
			"action failed without error",
			Outcome{Kind: OutcomeActionFailed, Action: ActionKill, Culprit: web, AvailablePct: 3},
			"memory/guard/web-app", health.ModeSMS,
			"memory: could not kill web-app (31 GB): unknown error; host 3% available",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cfg.Message(tt.out)
			if got.Key != tt.key || got.Mode != tt.mode || got.Text != tt.text {
				t.Errorf("Message() =\n%+v\nwant key %q mode %q text %q", got, tt.key, tt.mode, tt.text)
			}
		})
	}
}

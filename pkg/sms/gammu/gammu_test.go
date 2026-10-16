package gammu

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestPublishLogsAMaskedPhone(t *testing.T) {
	db, err := sql.Open("pgx", "postgres://gammu@db.example.org/gammu") // lazy: never connects
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	core, logs := observer.New(zap.InfoLevel)
	g := NewSQLBackend(zap.New(core), db)

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // the insert fails at once; only the log matters here
	if err = g.Publish(ctx, "+10000000099", "hello"); err == nil {
		t.Fatal("a cancelled insert must fail")
	}

	var found bool
	for _, e := range logs.All() {
		for _, f := range e.Context {
			if strings.Contains(f.String, "+10000000099") {
				t.Fatalf("log %q exposes the phone: %q", e.Message, f.String)
			}
			if f.Key == "phone" && f.String == "**********99" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("logs = %+v, want the masked phone", logs.All())
	}
}

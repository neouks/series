package db

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"
)

// Runs only against a disposable PostgreSQL instance. The test creates and
// drops databases, so it must never pick up an ordinary application DSN.
func TestTrafficRetentionUpgradeAndScopeSentryCleanup(t *testing.T) {
	if os.Getenv("SERIES_TEST_DB_ISOLATED") != "1" {
		t.Skip("requires SERIES_TEST_DB_ISOLATED=1 and a disposable PostgreSQL instance")
	}
	base, err := url.Parse(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	adminURL := *base
	adminURL.Path = "/postgres"
	admin, err := sql.Open("pgx", adminURL.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	for _, legacy := range []bool{false, true} {
		name := fmt.Sprintf("series_review_%d_%t", time.Now().UnixNano(), legacy)
		if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = admin.Exec(`DROP DATABASE IF EXISTS "` + name + `" WITH (FORCE)`) })
		installURL := *base
		installURL.Path = "/" + name
		if legacy {
			old, err := sql.Open("pgx", installURL.String())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := old.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
				old.Close()
				t.Fatal(err)
			}
			old.Close()
		}
		d, err := Open(installURL.String())
		if err != nil {
			t.Fatal(err)
		}
		want := "30"
		if legacy {
			want = "0"
		}
		if got, ok, err := d.GetSetting("traffic_retention_days"); err != nil || !ok || got != want {
			d.Close()
			t.Fatalf("legacy=%t retention=%q present=%t err=%v; want %s", legacy, got, ok, err, want)
		}
		if legacy {
			// Re-run the one-time cleanup against a configured instance first.
			if _, err := d.Exec(`INSERT INTO mcp_servers(name,transport,url,env,enabled) VALUES ('ScopeSentry','http','https://example.invalid/mcp','{"X-API-Key":"user-key"}',true)`); err != nil {
				d.Close()
				t.Fatal(err)
			}
			if _, err := d.Exec(`DELETE FROM settings WHERE key='scopesentry_placeholder_cleanup_v1'`); err != nil {
				d.Close()
				t.Fatal(err)
			}
			d.Close()
			d, err = Open(installURL.String())
			if err != nil {
				t.Fatal(err)
			}
			var count int
			if err := d.QueryRow(`SELECT count(*) FROM mcp_servers WHERE name='ScopeSentry' AND enabled`).Scan(&count); err != nil || count != 1 {
				d.Close()
				t.Fatalf("configured MCP was deleted: count=%d err=%v", count, err)
			}
			if _, err := d.Exec(`UPDATE mcp_servers SET url=NULL,env='{"X-API-Key":""}',enabled=false WHERE name='ScopeSentry'`); err != nil {
				d.Close()
				t.Fatal(err)
			}
			if _, err := d.Exec(`DELETE FROM settings WHERE key='scopesentry_placeholder_cleanup_v1'`); err != nil {
				d.Close()
				t.Fatal(err)
			}
			d.Close()
			d, err = Open(installURL.String())
			if err != nil {
				t.Fatal(err)
			}
			if err := d.QueryRow(`SELECT count(*) FROM mcp_servers WHERE name='ScopeSentry'`).Scan(&count); err != nil || count != 0 {
				d.Close()
				t.Fatalf("untouched placeholder survived: count=%d err=%v", count, err)
			}
		}
		d.Close()
	}
}

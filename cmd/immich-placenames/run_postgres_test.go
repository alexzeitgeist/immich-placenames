package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/alexzeitgeist/immich-placenames/internal/geocode"
	"github.com/alexzeitgeist/immich-placenames/internal/immich"
)

// Each connection sees only its temporary fixture tables, including after
// page commits. No library rows are selected or changed.
func runFixture(t *testing.T, n int) *pgx.Conn {
	t.Helper()
	dsn := os.Getenv("IMMICH_DSN")
	if dsn == "" {
		if os.Getenv("IMMICH_TEST_DB") != "1" {
			t.Skip("IMMICH_DSN or IMMICH_TEST_DB=1 required")
		}
		cfg, err := immich.ConfigFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		dsn = cfg.DSN()
	}
	ctx := context.Background()
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(context.Background()) })
	_, err = db.Exec(ctx, `SET search_path = pg_temp;
CREATE TEMP TABLE asset (id uuid PRIMARY KEY, "createdAt" timestamptz NOT NULL, "deletedAt" timestamptz);
CREATE TEMP TABLE asset_exif ("assetId" uuid PRIMARY KEY, latitude double precision, longitude double precision,
city varchar CHECK (city <> 'fail'), state varchar, country varchar, "lockedProperties" varchar[]);
CREATE TEMP TABLE system_metadata (key varchar PRIMARY KEY, value jsonb NOT NULL);`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(ctx, `INSERT INTO asset SELECT md5(i::text)::uuid,
TIMESTAMPTZ '2026-01-01' + i * INTERVAL '1 microsecond', NULL FROM generate_series(1, $1::int) i`, n)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(ctx, `INSERT INTO asset_exif ("assetId", latitude, longitude)
SELECT id, (row_number() OVER (ORDER BY "createdAt", id) - 1) % 80 + 1, 0 FROM asset`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestRunPostgresPageFailure(t *testing.T) {
	for _, size := range []int{0, 2} {
		for _, cancelRun := range []bool{false, true} {
			db := runFixture(t, 4)
			ctx, cancel := context.WithCancel(context.Background())
			r, err := quiet(t.TempDir()).processRun(ctx, databaseStore{db}, resolveFunc(func(ctx context.Context, p geocode.Point) (geocode.Result, error) {
				if db.PgConn().TxStatus() != 'I' {
					t.Fatal("resolution inside transaction")
				}
				res, err := namedPoint(ctx, p)
				if p.Lat == 4 {
					if cancelRun {
						cancel()
					} else {
						res.City = "fail"
					}
				}
				return res, err
			}), runOptions{PageSize: size})
			cancel()
			want := 0
			if size == 2 {
				want = 2
			}
			if err == nil || cancelRun && !errors.Is(err, context.Canceled) || r.Written != want || r.CommittedPages != want/2 {
				t.Fatalf("size=%d cancellation=%t report=%+v error=%v", size, cancelRun, r, err)
			}
			var stored int
			if err := db.QueryRow(context.Background(), `SELECT count(*) FROM asset_exif WHERE city IS NOT NULL`).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if stored != want {
				t.Fatalf("size=%d cancellation=%t stored=%d want=%d", size, cancelRun, stored, want)
			}
		}
	}
}

type timedStore struct {
	databaseStore
	durations []time.Duration
}

func (s *timedStore) Write(ctx context.Context, updates []immich.Update, all, lock bool) (immich.PageWrite, error) {
	r, err := s.databaseStore.Write(ctx, updates, all, lock)
	if err == nil {
		s.durations = append(s.durations, r.Duration)
	}
	return r, err
}

func TestPageTransactionTimings(t *testing.T) {
	if os.Getenv("IMMICH_BENCH") != "1" {
		t.Skip("IMMICH_BENCH=1 required")
	}
	db := runFixture(t, 10000)
	for _, size := range []int{0, 1000} {
		s := &timedStore{databaseStore: databaseStore{db}}
		start := time.Now()
		r, err := quiet(t.TempDir()).processRun(context.Background(), s, resolveFunc(namedPoint), runOptions{PageSize: size, All: true})
		if err != nil || r.Selected != 10000 || r.Written != 10000 || r.Changed != 0 {
			t.Fatalf("report %+v error %v", r, err)
		}
		var total, longest time.Duration
		for _, d := range s.durations {
			total += d
			if d > longest {
				longest = d
			}
		}
		t.Logf("page_size=%d assets=%d commits=%d transaction_total=%s transaction_max=%s pass=%s", size, r.Selected, len(s.durations), total, longest, time.Since(start))
		wantPages := 1
		if size > 0 {
			wantPages = 10
		}
		if r.CommittedPages != wantPages {
			t.Fatalf("committed %d pages, want %d", r.CommittedPages, wantPages)
		}
	}
}

func TestWarnReverseGeocoding(t *testing.T) {
	db := runFixture(t, 1)
	ctx := context.Background()
	for _, tc := range []struct {
		name, value, want, level string
	}{
		{"no row", "", "no database setting; Immich default is enabled", "WARN"},
		{"missing setting", `{"reverseGeocoding": {}}`, "no database setting; Immich default is enabled", "WARN"},
		{"enabled", `{"reverseGeocoding": {"enabled": true}}`, "database setting enabled=true", "WARN"},
		// A stored false may be stale when Immich uses IMMICH_CONFIG_FILE.
		{"disabled", `{"reverseGeocoding": {"enabled": false}}`, "database setting enabled=false", "INFO"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.Exec(ctx, `TRUNCATE system_metadata`); err != nil {
				t.Fatal(err)
			}
			if tc.value != "" {
				if _, err := db.Exec(ctx, `INSERT INTO system_metadata (key, value) VALUES ('system-config', $1::jsonb)`, tc.value); err != nil {
					t.Fatal(err)
				}
			}
			var logs strings.Builder
			a := quiet(t.TempDir())
			a.log = slog.New(slog.NewTextHandler(&logs, nil))
			a.warnReverseGeocoding(ctx, db)
			for _, want := range []string{"immich reverse geocoding", tc.want, "level=" + tc.level,
				"effective setting unknown", "IMMICH_CONFIG_FILE overrides database settings",
				"if enabled", "a run without -all skips named assets"} {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("logs %q lack %q", logs.String(), want)
				}
			}
		})
	}

	// An unreadable setting warns instead of stopping the run.
	if _, err := db.Exec(ctx, `DROP TABLE system_metadata`); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	a := quiet(t.TempDir())
	a.log = slog.New(slog.NewTextHandler(&logs, nil))
	a.warnReverseGeocoding(ctx, db)
	if !strings.Contains(logs.String(), "immich reverse geocoding unread") {
		t.Fatalf("missing table not reported: %s", logs.String())
	}
}

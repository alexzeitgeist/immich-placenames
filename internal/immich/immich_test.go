package immich

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type fakeBeginner struct {
	tx    pgx.Tx
	err   error
	calls int
}

func (b *fakeBeginner) Begin(context.Context) (pgx.Tx, error) {
	b.calls++
	return b.tx, b.err
}

type fakeTx struct {
	pgx.Tx
	affected      []int64
	execCount     int
	execErrAt     int
	execErr       error
	onExec        func()
	commitCalls   int
	commitErr     error
	rollbackCalls int
	rollbackErr   error
	rollbackCtx   context.Context
}

func (tx *fakeTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	tx.execCount++
	if tx.onExec != nil {
		tx.onExec()
	}
	if tx.execErrAt == tx.execCount {
		return pgconn.CommandTag{}, tx.execErr
	}
	affected := int64(1)
	if tx.execCount <= len(tx.affected) {
		affected = tx.affected[tx.execCount-1]
	}
	return pgconn.NewCommandTag(fmt.Sprintf("UPDATE %d", affected)), nil
}

func (tx *fakeTx) Commit(context.Context) error {
	tx.commitCalls++
	return tx.commitErr
}

func (tx *fakeTx) Rollback(ctx context.Context) error {
	tx.rollbackCalls++
	tx.rollbackCtx = ctx
	return tx.rollbackErr
}

func testUpdate(id string) Update {
	return Update{Asset: Asset{ID: id}, City: "city-" + id, Country: "country-" + id}
}

func TestConfigFromEnvDefaults(t *testing.T) {
	t.Setenv("DB_USERNAME", "postgres")
	t.Setenv("DB_PASSWORD", "secret")
	t.Setenv("DB_DATABASE_NAME", "immich")
	t.Setenv("DB_HOSTNAME", "")
	t.Setenv("DB_PORT", "")
	t.Setenv("DB_URL", "")

	// Empty host and port use the connection defaults.
	got, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != "database" {
		t.Fatalf("Host=%q, want database", got.Host)
	}
	if got.Port != "5432" {
		t.Fatalf("Port=%q, want 5432", got.Port)
	}

	t.Setenv("DB_HOSTNAME", "127.0.0.1")
	t.Setenv("DB_PORT", "15432")
	got, err = ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != "127.0.0.1" || got.Port != "15432" {
		t.Fatalf("Host=%q Port=%q, want 127.0.0.1 15432", got.Host, got.Port)
	}
}

func TestConfigFromEnvURL(t *testing.T) {
	const dsn = "postgres://immich:secret@pg.example.com:6543/photos?sslmode=require"
	t.Setenv("DB_URL", dsn)
	t.Setenv("DB_HOSTNAME", "")
	t.Setenv("DB_PORT", "")
	t.Setenv("DB_USERNAME", "")
	t.Setenv("DB_PASSWORD", "")
	t.Setenv("DB_DATABASE_NAME", "")

	got, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != "pg.example.com" || got.Port != "6543" || got.User != "immich" || got.Database != "photos" {
		t.Fatalf("Host=%q Port=%q User=%q Database=%q", got.Host, got.Port, got.User, got.Database)
	}
	if got.DSN() != dsn {
		t.Fatalf("DSN=%q, want %q", got.DSN(), dsn)
	}

	// DB_URL overrides the individual variables.
	t.Setenv("DB_HOSTNAME", "other.example.com")
	t.Setenv("DB_PORT", "5432")
	t.Setenv("DB_USERNAME", "other")
	t.Setenv("DB_PASSWORD", "other")
	t.Setenv("DB_DATABASE_NAME", "other")
	if got, err = ConfigFromEnv(); err != nil || got.Host != "pg.example.com" || got.DSN() != dsn {
		t.Fatalf("Host=%q DSN=%q: %v", got.Host, got.DSN(), err)
	}

	// Invalid DB_URL must fail even when the individual variables are valid.
	t.Setenv("DB_URL", "postgres://immich@pg.example.com:none/photos")
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "DB_URL") {
		t.Fatalf("err=%v, want a DB_URL error", err)
	}
}

func TestConfigFromEnvURLLibpqCompat(t *testing.T) {
	t.Setenv("PGSSLMODE", "")
	t.Setenv("PGSSLROOTCERT", "")
	for _, mode := range []string{"require", "verify-full", "disable"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("DB_URL", "postgresql://immich:s%40cret@pg.example.com:6543/photos?sslmode="+mode+"&uselibpqcompat=true&application_name=place%20names")
			cfg, err := ConfigFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := pgx.ParseConfig(cfg.DSN())
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := parsed.RuntimeParams["uselibpqcompat"]; ok {
				t.Fatal("client-only option would be sent to PostgreSQL")
			}
			if parsed.Host != "pg.example.com" || parsed.Port != 6543 || parsed.User != "immich" || parsed.Password != "s@cret" || parsed.Database != "photos" || parsed.RuntimeParams["application_name"] != "place names" {
				t.Fatal("normalization changed connection settings")
			}
			if mode == "disable" {
				if parsed.TLSConfig != nil {
					t.Fatal("sslmode=disable enabled TLS")
				}
				return
			}
			if parsed.TLSConfig == nil || len(parsed.Fallbacks) != 0 {
				t.Fatal("connection must require TLS without plaintext fallback")
			}
			if parsed.TLSConfig.InsecureSkipVerify != (mode == "require") {
				t.Fatalf("incorrect certificate verification for sslmode=%s", mode)
			}
		})
	}
}

func TestConfigFromEnvConnectionStrings(t *testing.T) {
	for _, tt := range []struct {
		name, dsn, want string
	}{
		{"keywords", "host=pg.example.com port=6543 user=immich password='a b' dbname=photos sslmode=require", ""},
		{"unchanged", "postgres://immich:secret@pg.example.com/photos?application_name=a+b%20c&sslmode=require", ""},
		{"query", "postgres://immich:secret@pg.example.com/photos?application_name=first&uselibpqcompat=true&application_name=a+b%20c&use%6cibpqcompat=true&sslmode=require", "postgres://immich:secret@pg.example.com/photos?application_name=first&application_name=a+b%20c&sslmode=require"},
		{"multiple hosts", "postgres://immich:secret@[::1]:5432,[::2]:5433/photos?uselibpqcompat=true&sslmode=require", "postgres://immich:secret@[::1]:5432,[::2]:5433/photos?sslmode=require"},
		{"password", "postgres://immich:a?b@pg.example.com/photos?uselibpqcompat=true&sslmode=require", "postgres://immich:a?b@pg.example.com/photos?sslmode=require"},
		{"only option", "postgresql://immich:secret@pg.example.com/photos?uselibpqcompat=true", "postgresql://immich:secret@pg.example.com/photos?"},
		{"no query", "postgresql://immich:secret@pg.example.com/photos", ""},
		{"spaces", "postgres://immich:secret@pg.example.com/photos? uselibpqcompat =true&sslmode=require", "postgres://immich:secret@pg.example.com/photos?sslmode=require"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DB_URL", tt.dsn)
			cfg, err := ConfigFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			want := tt.want
			if want == "" {
				want = tt.dsn
			}
			if cfg.DSN() != want {
				t.Fatalf("DSN=%q, want %q", cfg.DSN(), want)
			}
			parsed, err := pgx.ParseConfig(cfg.DSN())
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := parsed.RuntimeParams["uselibpqcompat"]; ok {
				t.Fatal("uselibpqcompat remains in connection parameters")
			}
		})
	}
}

func TestWritePageCountsWrittenAndChanged(t *testing.T) {
	tx := &fakeTx{affected: []int64{1, 0, 1}}
	db := &fakeBeginner{tx: tx}
	updates := []Update{testUpdate("first"), testUpdate("changed"), testUpdate("last")}

	got, err := WritePage(context.Background(), db, updates, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Written != 2 {
		t.Fatalf("Written=%d, want 2", got.Written)
	}
	if !slices.Equal(got.Changed, []string{"changed"}) {
		t.Fatalf("Changed=%v, want [changed]", got.Changed)
	}
	if tx.commitCalls != 1 || tx.rollbackCalls != 0 {
		t.Fatalf("commit calls=%d, rollback calls=%d", tx.commitCalls, tx.rollbackCalls)
	}
}

func TestWritePageRollbackDisposition(t *testing.T) {
	writeErr := errors.New("write failed")
	tx := &fakeTx{execErrAt: 2, execErr: writeErr}
	db := &fakeBeginner{tx: tx}
	updates := []Update{testUpdate("first"), testUpdate("bad-id")}

	got, err := WritePage(context.Background(), db, updates, true, false)
	if !reflect.DeepEqual(got, PageWrite{}) {
		t.Fatalf("result=%+v, want zero result", got)
	}
	if !errors.Is(err, writeErr) {
		t.Fatalf("error=%v, want wrapped write error", err)
	}
	if !strings.Contains(err.Error(), `write asset "bad-id"`) || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("error=%q lacks asset and rollback disposition", err)
	}
	if tx.commitCalls != 0 || tx.rollbackCalls != 1 {
		t.Fatalf("commit calls=%d, rollback calls=%d", tx.commitCalls, tx.rollbackCalls)
	}
	deadline, ok := tx.rollbackCtx.Deadline()
	if !ok || time.Until(deadline) > 5*time.Second {
		t.Fatalf("rollback context deadline=%v, want independent five-second timeout", deadline)
	}

	cleanupErr := errors.New("rollback unavailable")
	tx = &fakeTx{execErrAt: 1, execErr: writeErr, rollbackErr: cleanupErr}
	db = &fakeBeginner{tx: tx}
	_, err = WritePage(context.Background(), db, []Update{testUpdate("cleanup-fails")}, true, false)
	if !errors.Is(err, writeErr) || !strings.Contains(err.Error(), "rollback failed") || strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("error=%q, want write error with failed-cleanup disposition", err)
	}
}

func TestWritePageContextAndEmptyInput(t *testing.T) {
	db := &fakeBeginner{tx: &fakeTx{}}
	got, err := WritePage(context.Background(), db, nil, true, false)
	if err != nil || !reflect.DeepEqual(got, PageWrite{}) || db.calls != 0 {
		t.Fatalf("empty input: result=%+v err=%v begin calls=%d", got, err, db.calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err = WritePage(ctx, db, []Update{testUpdate("canceled")}, true, false)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, PageWrite{}) || db.calls != 0 {
		t.Fatalf("canceled before begin: result=%+v err=%v begin calls=%d", got, err, db.calls)
	}

	ctx, cancel = context.WithCancel(context.Background())
	tx := &fakeTx{onExec: cancel}
	db = &fakeBeginner{tx: tx}
	got, err = WritePage(ctx, db, []Update{testUpdate("cancel-after-write")}, true, false)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, PageWrite{}) {
		t.Fatalf("canceled before commit: result=%+v err=%v", got, err)
	}
	if tx.commitCalls != 0 || tx.rollbackCalls != 1 {
		t.Fatalf("commit calls=%d, rollback calls=%d", tx.commitCalls, tx.rollbackCalls)
	}
	if !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("error=%q lacks rollback disposition", err)
	}
}

func TestWritePageCommitUnconfirmed(t *testing.T) {
	commitErr := errors.New("connection lost")
	tx := &fakeTx{commitErr: commitErr}
	db := &fakeBeginner{tx: tx}

	got, err := WritePage(context.Background(), db, []Update{testUpdate("commit")}, true, false)
	if !reflect.DeepEqual(got, PageWrite{}) {
		t.Fatalf("result=%+v, want zero result", got)
	}
	if !errors.Is(err, commitErr) || !strings.Contains(err.Error(), "commit unconfirmed") {
		t.Fatalf("error=%v, want unconfirmed commit error", err)
	}
	if strings.Contains(err.Error(), "rolled back") || strings.Contains(err.Error(), "rollback failed") {
		t.Fatalf("error=%q claims a commit outcome", err)
	}
	if tx.commitCalls != 1 || tx.rollbackCalls != 1 {
		t.Fatalf("commit calls=%d, rollback calls=%d", tx.commitCalls, tx.rollbackCalls)
	}
}

var testSchemaSequence uint64

type fixtureAsset struct {
	ID        string
	CreatedAt time.Time
	Lat, Lon  float64
	City      *string
	State     *string
	Country   *string
	DeletedAt *time.Time
	Locked    []string
}

type testFixture struct {
	conn   *pgx.Conn
	dsn    string
	schema string
	ident  string
}

func testDSN(t *testing.T) string {
	t.Helper()
	if dsn := os.Getenv("IMMICH_DSN"); dsn != "" {
		return dsn
	}
	if os.Getenv("IMMICH_TEST_DB") == "1" {
		cfg, err := ConfigFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		return cfg.DSN()
	}
	t.Skip("IMMICH_DSN or IMMICH_TEST_DB=1 not set")
	return ""
}

func newTestFixture(t *testing.T) *testFixture {
	t.Helper()
	ctx := context.Background()
	dsn := testDSN(t)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("immich_test_%d_%d", os.Getpid(), atomic.AddUint64(&testSchemaSequence, 1))
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
		_ = conn.Close(ctx)
		t.Fatal(err)
	}
	f := &testFixture{conn: conn, dsn: dsn, schema: schema, ident: ident}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(cleanupCtx, "DROP SCHEMA "+ident+" CASCADE")
		_ = conn.Close(cleanupCtx)
	})
	if _, err := conn.Exec(ctx, "SET search_path TO "+ident); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `
CREATE TABLE asset (
	id uuid PRIMARY KEY,
	"createdAt" timestamp(6) with time zone NOT NULL,
	"deletedAt" timestamp(6) with time zone
);
CREATE TABLE asset_exif (
	"assetId" uuid PRIMARY KEY REFERENCES asset(id),
	latitude double precision,
	longitude double precision,
	city varchar,
	state varchar,
	country varchar,
	"lockedProperties" varchar[] NOT NULL DEFAULT '{}'::varchar[]
)`); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *testFixture) connect(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(context.Background(), "SET search_path TO "+f.ident); err != nil {
		_ = conn.Close(context.Background())
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return *value
}

func (f *testFixture) seed(t *testing.T, rows []fixtureAsset) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.conn.Exec(ctx, `TRUNCATE TABLE asset_exif, asset`); err != nil {
		t.Fatal(err)
	}
	assetRows := make([][]any, len(rows))
	exifRows := make([][]any, len(rows))
	for i, row := range rows {
		assetRows[i] = []any{row.ID, row.CreatedAt, nullableTime(row.DeletedAt)}
		locked := row.Locked
		if locked == nil {
			locked = []string{}
		}
		exifRows[i] = []any{row.ID, row.Lat, row.Lon, nullableString(row.City), nullableString(row.State), nullableString(row.Country), locked}
	}
	if len(rows) == 0 {
		return
	}
	if _, err := f.conn.CopyFrom(ctx, pgx.Identifier{"asset"}, []string{"id", "createdAt", "deletedAt"}, pgx.CopyFromRows(assetRows)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.conn.CopyFrom(ctx, pgx.Identifier{"asset_exif"}, []string{"assetId", "latitude", "longitude", "city", "state", "country", "lockedProperties"}, pgx.CopyFromRows(exifRows)); err != nil {
		t.Fatal(err)
	}
}

func (f *testFixture) insert(t *testing.T, row fixtureAsset) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.conn.Exec(ctx, `INSERT INTO asset (id, "createdAt", "deletedAt") VALUES ($1, $2, $3)`, row.ID, row.CreatedAt, nullableTime(row.DeletedAt)); err != nil {
		t.Fatal(err)
	}
	locked := row.Locked
	if locked == nil {
		locked = []string{}
	}
	if _, err := f.conn.Exec(ctx, `INSERT INTO asset_exif ("assetId", latitude, longitude, city, state, country, "lockedProperties") VALUES ($1, $2, $3, $4, $5, $6, $7)`, row.ID, row.Lat, row.Lon, nullableString(row.City), nullableString(row.State), nullableString(row.Country), locked); err != nil {
		t.Fatal(err)
	}
}

func testID(n int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
}

func testCursor(asset Asset) *Cursor {
	return &Cursor{CreatedAt: asset.CreatedAt, ID: asset.ID}
}

func assetIDs(assets []Asset) []string {
	ids := make([]string, len(assets))
	for i, asset := range assets {
		ids[i] = asset.ID
	}
	return ids
}

func TestSelectionEndAndSelectPage(t *testing.T) {
	f := newTestFixture(t)
	ctx := context.Background()
	if end, err := SelectionEnd(ctx, f.conn, false); err != nil {
		t.Fatal(err)
	} else if end != nil {
		t.Fatalf("empty selection end=%+v, want nil", end)
	}

	same := time.Date(2026, 9, 13, 12, 0, 0, 123456000, time.UTC)
	later := same.Add(time.Second)
	latest := later.Add(time.Second)
	f.seed(t, []fixtureAsset{
		{ID: testID(1), CreatedAt: same, Lat: 1, Lon: 1},
		{ID: testID(2), CreatedAt: same, Lat: 2, Lon: 2},
		{ID: testID(3), CreatedAt: same, Lat: 3, Lon: 3, City: stringPtr("already named")},
		{ID: testID(4), CreatedAt: later, Lat: 4, Lon: 4},
		{ID: testID(5), CreatedAt: later, Lat: 5, Lon: 5, Country: stringPtr("already named")},
		{ID: testID(6), CreatedAt: latest, Lat: 6, Lon: 6},
	})

	end, err := SelectionEnd(ctx, f.conn, false)
	if err != nil {
		t.Fatal(err)
	}
	if end == nil || end.ID != testID(6) || !end.CreatedAt.Equal(latest) {
		t.Fatalf("selection end=%+v, want (%s,%s)", end, latest, testID(6))
	}
	allEnd, err := SelectionEnd(ctx, f.conn, true)
	if err != nil {
		t.Fatal(err)
	}
	if allEnd == nil || allEnd.ID != testID(6) {
		t.Fatalf("all selection end=%+v, want %s", allEnd, testID(6))
	}

	through := &Cursor{CreatedAt: same, ID: testID(2)}
	assets, err := SelectPage(ctx, f.conn, false, nil, through, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{testID(1), testID(2)}; !slices.Equal(assetIDs(assets), want) {
		t.Fatalf("through page=%v, want %v", assetIDs(assets), want)
	}
	assets, err = SelectPage(ctx, f.conn, false, testCursor(assets[0]), through, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{testID(2)}; !slices.Equal(assetIDs(assets), want) {
		t.Fatalf("exclusive after page=%v, want %v", assetIDs(assets), want)
	}
	assets, err = SelectPage(ctx, f.conn, false, nil, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{testID(1), testID(2)}; !slices.Equal(assetIDs(assets), want) {
		t.Fatalf("limited page=%v, want %v", assetIDs(assets), want)
	}
	if !assets[0].CreatedAt.Equal(same) || !assets[1].CreatedAt.Equal(same) {
		t.Fatalf("same-timestamp rows were not preserved: %v, %v", assets[0].CreatedAt, assets[1].CreatedAt)
	}

	// Keep the end cursor from before this row appeared. It must exclude the
	// later row while rows before it disappear from the unprocessed selection.
	f.insert(t, fixtureAsset{ID: testID(7), CreatedAt: latest.Add(time.Second), Lat: 7, Lon: 7})
	first, err := SelectPage(ctx, f.conn, false, nil, end, 2)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{testID(1), testID(2)}; !slices.Equal(assetIDs(first), want) {
		t.Fatalf("first page=%v, want %v", assetIDs(first), want)
	}
	if _, err := f.conn.Exec(ctx, `UPDATE asset_exif SET city = 'resolved', country = 'resolved' WHERE "assetId" = ANY($1::uuid[])`, []string{testID(1), testID(2)}); err != nil {
		t.Fatal(err)
	}

	var paged []string
	paged = append(paged, assetIDs(first)...)
	after := testCursor(first[len(first)-1])
	for {
		page, err := SelectPage(ctx, f.conn, false, after, end, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		paged = append(paged, assetIDs(page)...)
		after = testCursor(page[len(page)-1])
	}
	if want := []string{testID(1), testID(2), testID(4), testID(6)}; !slices.Equal(paged, want) {
		t.Fatalf("paged unprocessed selection=%v, want %v", paged, want)
	}

	assets, err = SelectPage(ctx, f.conn, false, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{testID(4), testID(6), testID(7)}; !slices.Equal(assetIDs(assets), want) {
		t.Fatalf("unbounded unprocessed selection=%v, want %v", assetIDs(assets), want)
	}
	assets, err = SelectPage(ctx, f.conn, true, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{testID(1), testID(2), testID(3), testID(4), testID(5), testID(6), testID(7)}; !slices.Equal(assetIDs(assets), want) {
		t.Fatalf("all selection=%v, want %v", assetIDs(assets), want)
	}
}

func (f *testFixture) names(t *testing.T, id string) (city, state, country string, locked []string) {
	t.Helper()
	err := f.conn.QueryRow(context.Background(), `SELECT coalesce(city, ''), coalesce(state, ''), coalesce(country, ''), "lockedProperties" FROM asset_exif WHERE "assetId" = $1::uuid`, id).Scan(&city, &state, &country, &locked)
	if err != nil {
		t.Fatal(err)
	}
	return city, state, country, locked
}

func stringPtr(value string) *string {
	return &value
}

func TestWritePageSuccess(t *testing.T) {
	f := newTestFixture(t)
	ts := time.Date(2026, 9, 13, 13, 0, 0, 0, time.UTC)
	f.seed(t, []fixtureAsset{
		{ID: testID(10), CreatedAt: ts, Lat: 10, Lon: 10},
		{ID: testID(11), CreatedAt: ts, Lat: 11, Lon: 11},
	})
	assets, err := SelectPage(context.Background(), f.conn, true, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	updates := []Update{
		{Asset: assets[0], City: "First City", Country: "Country"},
		{Asset: assets[1], City: "Second City", State: "State", Country: "Country"},
	}
	got, err := WritePage(context.Background(), f.conn, updates, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Written != 2 || len(got.Changed) != 0 || got.Duration < 0 {
		t.Fatalf("result=%+v, want two writes and no changes", got)
	}
	for _, want := range updates {
		city, state, country, locked := f.names(t, want.ID)
		if city != want.City || state != want.State || country != want.Country {
			t.Errorf("asset %s stored %q, %q, %q", want.ID, city, state, country)
		}
		for _, property := range []string{"city", "state", "country"} {
			if !slices.Contains(locked, property) {
				t.Errorf("asset %s locks=%v, missing %s", want.ID, locked, property)
			}
		}
	}
}

func TestWriteEmptyCity(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(fmt.Sprintf("all=%t", all), func(t *testing.T) {
			f := newTestFixture(t)
			row := fixtureAsset{ID: testID(12), CreatedAt: time.Now(), Lat: 12, Lon: 12}
			if all {
				row.City, row.State, row.Country = stringPtr("Old City"), stringPtr("Old State"), stringPtr("Old Country")
			}
			f.seed(t, []fixtureAsset{row})
			ctx := context.Background()
			assets, err := SelectPage(ctx, f.conn, all, nil, nil, 1)
			if err != nil || len(assets) != 1 {
				t.Fatalf("SelectPage = %v, %v", assets, err)
			}
			written, err := Write(ctx, f.conn, Update{Asset: assets[0], Country: "Country"}, all, true)
			if err != nil || !written {
				t.Fatalf("Write = %t, %v", written, err)
			}
			var city, state *string
			var country string
			var locked []string
			err = f.conn.QueryRow(ctx, `SELECT city, state, country, "lockedProperties" FROM asset_exif WHERE "assetId" = $1::uuid`, row.ID).Scan(&city, &state, &country, &locked)
			if err != nil {
				t.Fatal(err)
			}
			if city != nil || state != nil || country != "Country" {
				t.Fatalf("stored city=%v state=%v country=%q; want NULL, NULL, Country", city, state, country)
			}
			for _, property := range []string{"city", "state", "country"} {
				if !slices.Contains(locked, property) {
					t.Errorf("locks=%v, missing %s", locked, property)
				}
			}
			pending, err := SelectPage(ctx, f.conn, false, nil, nil, 0)
			if err != nil || len(pending) != 0 {
				t.Fatalf("pending after write = %v, %v", pending, err)
			}
		})
	}
}

func TestWriteConditionsAndLock(t *testing.T) {
	f := newTestFixture(t)
	ts := time.Date(2026, 9, 13, 14, 0, 0, 0, time.UTC)
	f.seed(t, []fixtureAsset{{ID: testID(20), CreatedAt: ts, Lat: 20, Lon: 20, Locked: []string{"dateTimeOriginal"}}})
	assets, err := SelectPage(context.Background(), f.conn, true, nil, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	u := Update{Asset: assets[0], City: "City", State: "State", Country: "Country"}

	moved := u
	moved.Lat += 0.5
	got, err := WritePage(context.Background(), f.conn, []Update{moved}, true, false)
	if err != nil || got.Written != 0 || !slices.Equal(got.Changed, []string{u.ID}) {
		t.Fatalf("moved coordinates: result=%+v err=%v", got, err)
	}
	city, state, country, _ := f.names(t, u.ID)
	if city != "" || state != "" || country != "" {
		t.Fatalf("moved-coordinate write changed names to %q, %q, %q", city, state, country)
	}

	got, err = WritePage(context.Background(), f.conn, []Update{u}, true, true)
	if err != nil || got.Written != 1 || len(got.Changed) != 0 {
		t.Fatalf("locked write: result=%+v err=%v", got, err)
	}
	city, state, country, locked := f.names(t, u.ID)
	if city != u.City || state != u.State || country != u.Country {
		t.Fatalf("locked write stored %q, %q, %q", city, state, country)
	}
	for _, property := range []string{"dateTimeOriginal", "city", "state", "country"} {
		if !slices.Contains(locked, property) {
			t.Errorf("locks=%v, missing %s", locked, property)
		}
	}

	got, err = WritePage(context.Background(), f.conn, []Update{u}, false, false)
	if err != nil || got.Written != 0 || !slices.Equal(got.Changed, []string{u.ID}) {
		t.Fatalf("names set with all=false: result=%+v err=%v", got, err)
	}
	if _, err := f.conn.Exec(context.Background(), `UPDATE asset SET "deletedAt" = now() WHERE id = $1::uuid`, u.ID); err != nil {
		t.Fatal(err)
	}
	got, err = WritePage(context.Background(), f.conn, []Update{u}, true, false)
	if err != nil || got.Written != 0 || !slices.Equal(got.Changed, []string{u.ID}) {
		t.Fatalf("soft-deleted: result=%+v err=%v", got, err)
	}
	// Reset still includes trashed rows and removes only the location locks.
	ctx := context.Background()
	if n, err := Count(ctx, f.conn, IDs([]string{u.ID})); err != nil || n != 1 {
		t.Fatalf("reset count: n=%d err=%v", n, err)
	}
	if n, err := Reset(ctx, f.conn, IDs([]string{u.ID})); err != nil || n != 1 {
		t.Fatalf("reset: n=%d err=%v", n, err)
	}
	city, state, country, locked = f.names(t, u.ID)
	if city != "" || state != "" || country != "" || !slices.Equal(locked, []string{"dateTimeOriginal"}) {
		t.Fatalf("reset left %q %q %q locks=%v", city, state, country, locked)
	}
	if _, err := f.conn.Exec(ctx, `UPDATE asset_exif SET "lockedProperties" = '{city,dateTimeOriginal}' WHERE "assetId" = $1::uuid`, u.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := Reset(ctx, f.conn, IDs([]string{u.ID})); err != nil || n != 1 {
		t.Fatalf("lock-only reset: n=%d err=%v", n, err)
	}
	if _, err := f.conn.Exec(ctx, `UPDATE asset_exif SET city = 'Nowhere' WHERE "assetId" = $1::uuid`, u.ID); err != nil {
		t.Fatal(err)
	}
	selection, err := Named("city", "Nowhere")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := Reset(ctx, f.conn, selection); err != nil || n != 1 {
		t.Fatalf("named reset on trashed row: n=%d err=%v", n, err)
	}
	if _, err := Named("bogus", "x"); err == nil {
		t.Fatal("unknown reset column accepted")
	}
}

func TestWritePagesRollbackOnlyActivePage(t *testing.T) {
	f := newTestFixture(t)
	ts := time.Date(2026, 9, 13, 15, 0, 0, 0, time.UTC)
	f.seed(t, []fixtureAsset{
		{ID: testID(30), CreatedAt: ts, Lat: 30, Lon: 30},
		{ID: testID(31), CreatedAt: ts, Lat: 31, Lon: 31},
		{ID: testID(32), CreatedAt: ts, Lat: 32, Lon: 32},
	})
	assets, err := SelectPage(context.Background(), f.conn, true, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	firstPage, err := WritePage(context.Background(), f.conn, []Update{{Asset: assets[0], City: "first", Country: "country"}}, false, false)
	if err != nil || firstPage.Written != 1 {
		t.Fatalf("first page: result=%+v err=%v", firstPage, err)
	}
	bad := testUpdate("not-a-uuid")
	secondPage, err := WritePage(context.Background(), f.conn, []Update{
		{Asset: assets[1], City: "second", Country: "country"},
		bad,
	}, false, false)
	if !reflect.DeepEqual(secondPage, PageWrite{}) {
		t.Fatalf("failed second page result=%+v, want zero", secondPage)
	}
	if err == nil || !strings.Contains(err.Error(), `write asset "not-a-uuid"`) || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("failed second page error=%v", err)
	}
	city, _, _, _ := f.names(t, assets[0].ID)
	if city != "first" {
		t.Fatalf("first page city=%q, want first", city)
	}
	city, _, _, _ = f.names(t, assets[1].ID)
	if city != "" {
		t.Fatalf("rolled-back second page city=%q, want empty", city)
	}

	atomicPage, err := WritePage(context.Background(), f.conn, []Update{
		{Asset: assets[2], City: "atomic", Country: "country"},
		bad,
	}, false, false)
	if !reflect.DeepEqual(atomicPage, PageWrite{}) || err == nil {
		t.Fatalf("atomic failure result=%+v err=%v", atomicPage, err)
	}
	city, _, _, _ = f.names(t, assets[2].ID)
	if city != "" {
		t.Fatalf("atomic rollback city=%q, want empty", city)
	}
}

func TestConcurrentOverlappingConditionalWrites(t *testing.T) {
	f := newTestFixture(t)
	ts := time.Date(2026, 9, 13, 16, 0, 0, 0, time.UTC)
	f.seed(t, []fixtureAsset{{ID: testID(40), CreatedAt: ts, Lat: 40, Lon: 40}})
	conn1 := f.connect(t)
	conn2 := f.connect(t)
	asset, err := SelectPage(context.Background(), f.conn, true, nil, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	u1 := Update{Asset: asset[0], City: "winner one", Country: "country one"}
	u2 := Update{Asset: asset[0], City: "winner two", Country: "country two"}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	type outcome struct {
		result PageWrite
		err    error
	}
	outcomes := make(chan outcome, 2)
	go func() {
		<-start
		result, err := WritePage(ctx, conn1, []Update{u1}, false, false)
		outcomes <- outcome{result: result, err: err}
	}()
	go func() {
		<-start
		result, err := WritePage(ctx, conn2, []Update{u2}, false, false)
		outcomes <- outcome{result: result, err: err}
	}()
	close(start)
	first, second := <-outcomes, <-outcomes
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent writes: first=%v second=%v", first.err, second.err)
	}
	if first.result.Written+second.result.Written != 1 || len(first.result.Changed)+len(second.result.Changed) != 1 {
		t.Fatalf("outcomes=%+v and %+v, want one write and one changed asset", first.result, second.result)
	}
	city, _, country, _ := f.names(t, u1.ID)
	if !((city == u1.City && country == u1.Country) || (city == u2.City && country == u2.Country)) {
		t.Fatalf("stored concurrent result=%q, %q", city, country)
	}
}

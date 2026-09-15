// Package immich selects assets and writes place names in Immich's database.
// It knows nothing about providers.
package immich

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Config is the connection, from the DB_* variables Immich's .env defines.
type Config struct {
	Host, Port, User, Password, Database string
}

// ConfigFromEnv reads DB_HOST (default database), DB_PORT (default 5432),
// DB_USERNAME, DB_PASSWORD and DB_DATABASE_NAME.
func ConfigFromEnv() (Config, error) {
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	c := Config{Host: env("DB_HOST", "database"), Port: env("DB_PORT", "5432"),
		User: os.Getenv("DB_USERNAME"), Password: os.Getenv("DB_PASSWORD"), Database: os.Getenv("DB_DATABASE_NAME")}
	if c.User == "" || c.Password == "" || c.Database == "" {
		return c, errors.New("DB_USERNAME, DB_PASSWORD and DB_DATABASE_NAME must be set")
	}
	return c, nil
}

// DSN is the connection URL.
func (c Config) DSN() string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword(c.User, c.Password), Host: net.JoinHostPort(c.Host, c.Port), Path: "/" + c.Database}
	return u.String()
}

// Connect opens one connection.
func Connect(ctx context.Context, dsn string) (*pgx.Conn, error) { return pgx.Connect(ctx, dsn) }

// Querier is what *pgx.Conn and pgx.Tx share.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Beginner starts a transaction. *pgx.Conn and pgxpool.Conn implement it.
type Beginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

// Asset is one asset with coordinates.
type Asset struct {
	ID        string
	CreatedAt time.Time
	Lat, Lon  float64
}

const selectFromSQL = `FROM asset a JOIN asset_exif e ON e."assetId" = a.id
WHERE e.latitude IS NOT NULL AND e.longitude IS NOT NULL AND a."deletedAt" IS NULL`

const selectSQL = `SELECT a.id::text, a."createdAt", e.latitude, e.longitude
` + selectFromSQL

// Cursor identifies an asset's position in created-time and UUID order.
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

// SelectionEnd returns the greatest eligible asset, or nil when there is no
// eligible asset. Its criteria are the same as SelectPage.
func SelectionEnd(ctx context.Context, q Querier, all bool) (*Cursor, error) {
	sql := `SELECT a."createdAt", a.id::text
` + selectFromSQL
	if !all {
		sql += ` AND e.city IS NULL AND e.country IS NULL`
	}
	sql += ` ORDER BY a."createdAt" DESC, a.id DESC LIMIT 1`
	var c Cursor
	err := q.QueryRow(ctx, sql).Scan(&c.CreatedAt, &c.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// SelectPage returns eligible assets in ascending created-time and UUID
// order. after is exclusive and through is inclusive. A zero limit is
// unbounded.
func SelectPage(ctx context.Context, q Querier, all bool, after, through *Cursor, limit int) ([]Asset, error) {
	sql := selectSQL
	if !all {
		sql += ` AND e.city IS NULL AND e.country IS NULL`
	}
	args := make([]any, 0, 5)
	if after != nil {
		start := len(args) + 1
		sql += fmt.Sprintf(` AND (a."createdAt", a.id) > ($%d, $%d::uuid)`, start, start+1)
		args = append(args, after.CreatedAt, after.ID)
	}
	if through != nil {
		start := len(args) + 1
		sql += fmt.Sprintf(` AND (a."createdAt", a.id) <= ($%d, $%d::uuid)`, start, start+1)
		args = append(args, through.CreatedAt, through.ID)
	}
	sql += ` ORDER BY a."createdAt" ASC, a.id ASC`
	if limit > 0 {
		sql += fmt.Sprintf(" LIMIT $%d", len(args)+1)
		args = append(args, limit)
	}
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Asset
	for rows.Next() {
		var a Asset
		if err := rows.Scan(&a.ID, &a.CreatedAt, &a.Lat, &a.Lon); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Unprocessed counts assets with coordinates and no city and country.
func Unprocessed(ctx context.Context, q Querier) (int64, error) {
	var n int64
	err := q.QueryRow(ctx, `SELECT count(*) FROM asset a JOIN asset_exif e ON e."assetId" = a.id
WHERE e.latitude IS NOT NULL AND e.longitude IS NOT NULL AND a."deletedAt" IS NULL AND e.city IS NULL AND e.country IS NULL`).Scan(&n)
	return n, err
}

// Update is one asset's names to write.
type Update struct {
	Asset
	City, State, Country string
}

// PageWrite is the result of one committed page transaction.
type PageWrite struct {
	Written  int
	Changed  []string
	Duration time.Duration
}

func rollbackPage(tx pgx.Tx) error {
	rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return tx.Rollback(rollbackCtx)
}

func rollbackDisposition(operation string, cause, rollbackErr error) error {
	if rollbackErr == nil {
		return fmt.Errorf("%s: %w (rolled back)", operation, cause)
	}
	return fmt.Errorf("%s: %w (rollback failed: %v)", operation, cause, rollbackErr)
}

// WritePage conditionally writes a page in one transaction. Successful
// conditional writes count in Written; a false result from Write means that
// the asset changed meanwhile and is reported in Changed. An error rolls the
// whole page back and returns a zero result. Commit errors are reported as
// unconfirmed because the commit outcome may be unknown.
func WritePage(ctx context.Context, db Beginner, updates []Update, all, lock bool) (PageWrite, error) {
	if len(updates) == 0 {
		return PageWrite{}, nil
	}
	if err := ctx.Err(); err != nil {
		return PageWrite{}, err
	}

	started := time.Now()
	tx, err := db.Begin(ctx)
	if err != nil {
		return PageWrite{}, err
	}
	finished := false
	defer func() {
		if !finished {
			_ = rollbackPage(tx)
		}
	}()
	rollback := func() error {
		err := rollbackPage(tx)
		finished = true
		return err
	}

	written := 0
	changed := make([]string, 0, len(updates))
	for _, u := range updates {
		if err := ctx.Err(); err != nil {
			return PageWrite{}, rollbackDisposition("write page", err, rollback())
		}
		ok, err := Write(ctx, tx, u, all, lock)
		if err != nil {
			return PageWrite{}, rollbackDisposition(fmt.Sprintf("write asset %q", u.ID), err, rollback())
		}
		if ok {
			written++
		} else {
			changed = append(changed, u.ID)
		}
	}
	if err := ctx.Err(); err != nil {
		return PageWrite{}, rollbackDisposition("write page", err, rollback())
	}
	if err := tx.Commit(ctx); err != nil {
		return PageWrite{}, fmt.Errorf("commit unconfirmed: %w", err)
	}
	finished = true
	return PageWrite{Written: written, Changed: changed, Duration: time.Since(started)}, nil
}

// Write sets the names when the asset is still not deleted, its coordinates
// are still the ones read and, unless all, its city and country are still
// null. An empty city or state is stored as NULL. With lock the three column
// names join "lockedProperties". False means the asset changed meanwhile.
func Write(ctx context.Context, q Querier, u Update, all, lock bool) (bool, error) {
	set := `city = $2, state = $3, country = $4`
	if lock {
		set += `, "lockedProperties" = array(SELECT DISTINCT unnest(coalesce(e."lockedProperties", '{}') || '{city,state,country}'))`
	}
	sql := `UPDATE asset_exif e SET ` + set + `
FROM asset a WHERE e."assetId" = a.id AND a.id = $1::uuid AND a."deletedAt" IS NULL AND e.latitude = $5 AND e.longitude = $6`
	if !all {
		sql += ` AND e.city IS NULL AND e.country IS NULL`
	}
	tag, err := q.Exec(ctx, sql, u.ID, nullable(u.City), nullable(u.State), u.Country, u.Lat, u.Lon)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Selection is the rows a reset touches: a condition over asset_exif e,
// with its arguments.
type Selection struct {
	where string
	args  []any
}

// All selects every row.
func All() Selection { return Selection{where: "TRUE"} }

// IDs selects the given assets.
func IDs(ids []string) Selection {
	return Selection{where: `e."assetId" = ANY($1::uuid[])`, args: []any{ids}}
}

// Named selects the rows whose city, state or country equals value, trashed
// assets included, as All and IDs do.
func Named(column, value string) (Selection, error) {
	switch column {
	case "city", "state", "country":
	default:
		return Selection{}, fmt.Errorf("reset: column %q", column)
	}
	return Selection{where: `e.` + column + ` = $1`, args: []any{value}}, nil
}

const clearSQL = `city = NULL, state = NULL, country = NULL,
"lockedProperties" = array_remove(array_remove(array_remove(e."lockedProperties", 'city'::varchar), 'state'::varchar), 'country'::varchar)`

// named restricts a selection to rows with any of the three names or any of
// their locks, so that a reset clears a lock left on a row whose names are
// already null.
const named = `(e.city IS NOT NULL OR e.state IS NOT NULL OR e.country IS NOT NULL OR e."lockedProperties" && '{city,state,country}'::varchar[])`

// Count reports how many rows Reset would clear.
func Count(ctx context.Context, q Querier, s Selection) (int64, error) {
	var n int64
	err := q.QueryRow(ctx, `SELECT count(*) FROM asset_exif e WHERE `+s.where+` AND `+named, s.args...).Scan(&n)
	return n, err
}

// Reset clears the names and their locks on the selection.
func Reset(ctx context.Context, q Querier, s Selection) (int64, error) {
	tag, err := q.Exec(ctx, `UPDATE asset_exif e SET `+clearSQL+` WHERE `+s.where+` AND `+named, s.args...)
	return tag.RowsAffected(), err
}

package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/alexzeitgeist/immich-placenames/internal/geocode"
	"github.com/alexzeitgeist/immich-placenames/internal/immich"
)

const defaultPageSize = 1000

type runOptions struct {
	All, Lock, DryRun bool
	Limit, PageSize   int
}

type runReport struct {
	Selected, Written, Reported int
	NoCountry, Changed, Failed  int
	CommittedPages              int
	Updates                     []immich.Update // retained only for the globally sorted dry-run CSV
}

// runStore keeps selection and transactions outside the resolver. Each Page
// finishes reading before resolution; Write returns only confirmed commits.
type runStore interface {
	End(context.Context, bool) (*immich.Cursor, error)
	Page(context.Context, bool, *immich.Cursor, *immich.Cursor, int) ([]immich.Asset, error)
	Write(context.Context, []immich.Update, bool, bool) (immich.PageWrite, error)
}

type databaseStore struct{ db *pgx.Conn }

func (s databaseStore) End(ctx context.Context, all bool) (*immich.Cursor, error) {
	return immich.SelectionEnd(ctx, s.db, all)
}

func (s databaseStore) Page(ctx context.Context, all bool, after, through *immich.Cursor, limit int) ([]immich.Asset, error) {
	return immich.SelectPage(ctx, s.db, all, after, through, limit)
}

func (s databaseStore) Write(ctx context.Context, updates []immich.Update, all, lock bool) (immich.PageWrite, error) {
	return immich.WritePage(ctx, s.db, updates, all, lock)
}

func (a *app) processRun(ctx context.Context, store runStore, resolver geocode.Resolver, opts runOptions) (runReport, error) {
	var report runReport
	var after, through *immich.Cursor
	if opts.PageSize > 0 {
		var err error
		through, err = store.End(ctx, opts.All)
		if err != nil {
			return report, fmt.Errorf("selection end: %w", err)
		}
		if through == nil {
			return report, nil
		}
	}
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		limit := opts.PageSize
		if opts.Limit > 0 {
			remaining := opts.Limit - report.Selected
			if remaining <= 0 {
				break
			}
			if limit == 0 || remaining < limit {
				limit = remaining
			}
		}
		assets, err := store.Page(ctx, opts.All, after, through, limit)
		if err != nil {
			return report, fmt.Errorf("select page %d: %w", page, err)
		}
		if len(assets) == 0 {
			break
		}
		report.Selected += len(assets)
		gassets := make([]geocode.Asset, len(assets))
		for i, as := range assets {
			gassets[i] = geocode.Asset{ID: as.ID, Point: geocode.Point{Lat: as.Lat, Lon: as.Lon}}
		}
		outcomes, err := geocode.ResolveAll(ctx, resolver, gassets)
		// Cancellation during the last resolver call must also discard the page.
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			return report, fmt.Errorf("resolve page %d, page not written: %w", page, err)
		}
		var updates []immich.Update
		for i, o := range outcomes {
			switch {
			case o.Err != nil:
				report.Failed++
				a.log.Error("asset failed", "asset", o.Asset.ID, "err", o.Err)
			case !o.Result.Writable():
				report.NoCountry++
				a.log.Warn("no country", "asset", o.Asset.ID, "lat", o.Asset.Point.Lat, "lon", o.Asset.Point.Lon)
			default:
				u := immich.Update{Asset: assets[i], City: o.Result.City, State: o.Result.State, Country: o.Result.Country}
				updates = append(updates, u)
				a.log.Debug("resolved", "asset", u.ID, "city", u.City, "state", u.State, "country", u.Country)
			}
		}
		if opts.DryRun {
			report.Updates = append(report.Updates, updates...)
		} else if len(updates) > 0 {
			committed, err := store.Write(ctx, updates, opts.All, opts.Lock)
			if err != nil {
				return report, fmt.Errorf("write page %d: %w", page, err)
			}
			report.Written += committed.Written
			report.Changed += len(committed.Changed)
			report.CommittedPages++
			for _, id := range committed.Changed {
				a.log.Warn("changed meanwhile", "asset", id)
			}
			a.log.Debug("page committed", "page", page, "selected", len(assets), "written", committed.Written,
				"changed", len(committed.Changed), "duration", committed.Duration)
		}
		last := assets[len(assets)-1]
		after = &immich.Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
		if opts.PageSize == 0 || len(assets) < limit {
			break
		}
	}
	return report, nil
}

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexzeitgeist/immich-placenames/internal/geocode"
	"github.com/alexzeitgeist/immich-placenames/internal/immich"
)

type resolveFunc func(context.Context, geocode.Point) (geocode.Result, error)

func (f resolveFunc) Resolve(ctx context.Context, pt geocode.Point) (geocode.Result, error) {
	return f(ctx, pt)
}

func namedPoint(_ context.Context, pt geocode.Point) (geocode.Result, error) {
	return geocode.Result{City: fmt.Sprintf("City %g", pt.Lat), State: "State", Country: "Country", Found: true}, nil
}

// memoryStore models changing eligibility after commits. SQL ordering and
// real transaction boundaries are exercised by internal/immich's DB tests.
type memoryStore struct {
	assets     []immich.Asset
	writes     [][]immich.Update
	limits     []int
	failPage   int
	afterWrite func()
}

func fixtureStore(n int) *memoryStore {
	s := &memoryStore{}
	for i := 1; i <= n; i++ {
		s.assets = append(s.assets, immich.Asset{ID: fmt.Sprintf("%03d", i), Lat: float64(i), CreatedAt: time.Unix(100, 0)})
	}
	return s
}

func (s *memoryStore) End(context.Context, bool) (*immich.Cursor, error) {
	if len(s.assets) == 0 {
		return nil, nil
	}
	a := s.assets[len(s.assets)-1]
	return &immich.Cursor{CreatedAt: a.CreatedAt, ID: a.ID}, nil
}

func (s *memoryStore) Page(_ context.Context, all bool, after, through *immich.Cursor, limit int) ([]immich.Asset, error) {
	s.limits = append(s.limits, limit)
	var out []immich.Asset
	for _, a := range s.assets {
		if after != nil && a.ID <= after.ID || through != nil && a.ID > through.ID {
			continue
		}
		written := false
		for _, page := range s.writes {
			for _, u := range page {
				written = written || u.ID == a.ID
			}
		}
		if !all && written {
			continue
		}
		out = append(out, a)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *memoryStore) Write(ctx context.Context, updates []immich.Update, _, _ bool) (immich.PageWrite, error) {
	if err := ctx.Err(); err != nil {
		return immich.PageWrite{}, err
	}
	if s.failPage == len(s.writes)+1 {
		return immich.PageWrite{}, errors.New("transaction rolled back")
	}
	s.writes = append(s.writes, updates)
	if s.afterWrite != nil {
		s.afterWrite()
	}
	return immich.PageWrite{Written: len(updates)}, nil
}

func TestRunPagesAndLimit(t *testing.T) {
	for _, tc := range []struct{ size, limit, selected, pages int }{
		{0, 0, 5, 1}, {1, 0, 5, 5}, {2, 0, 5, 3}, {5, 0, 5, 1}, {1000, 0, 5, 1},
		{2, 3, 3, 2}, {0, 3, 3, 1}, {2, 1, 1, 1},
	} {
		t.Run(fmt.Sprintf("size%d_limit%d", tc.size, tc.limit), func(t *testing.T) {
			s := fixtureStore(5)
			r, err := quiet(t.TempDir()).processRun(context.Background(), s, resolveFunc(namedPoint), runOptions{PageSize: tc.size, Limit: tc.limit})
			if err != nil || r.Selected != tc.selected || r.Written != tc.selected || r.CommittedPages != tc.pages || len(r.Updates) != 0 {
				t.Fatalf("report %+v error %v", r, err)
			}
		})
	}
}

func TestRunAdvancesPastUnwritablePage(t *testing.T) {
	s := fixtureStore(5)
	r, err := quiet(t.TempDir()).processRun(context.Background(), s, resolveFunc(func(ctx context.Context, p geocode.Point) (geocode.Result, error) {
		if p.Lat == 1 {
			return geocode.Result{}, errors.New("bad geometry")
		}
		if p.Lat == 2 {
			return geocode.Result{}, nil
		}
		return namedPoint(ctx, p)
	}), runOptions{PageSize: 2, Limit: 3})
	if err != nil || r.Selected != 3 || r.Failed != 1 || r.NoCountry != 1 || r.Written != 1 || r.CommittedPages != 1 || !reflect.DeepEqual(s.limits, []int{2, 1}) {
		t.Fatalf("report %+v error %v limits %v", r, err, s.limits)
	}
}

func TestRunEndBound(t *testing.T) {
	s := fixtureStore(2)
	s.afterWrite = func() { s.assets = append(s.assets, immich.Asset{ID: "003", Lat: 3}) }
	r, err := quiet(t.TempDir()).processRun(context.Background(), s, resolveFunc(namedPoint), runOptions{PageSize: 1, All: true})
	if err != nil || r.Selected != 2 || r.Written != 2 {
		t.Fatalf("report %+v error %v", r, err)
	}
}

func TestRunFailureCountsOnlyCommittedPages(t *testing.T) {
	s := fixtureStore(5)
	s.failPage = 2
	r, err := quiet(t.TempDir()).processRun(context.Background(), s, resolveFunc(namedPoint), runOptions{PageSize: 2})
	if err == nil || !strings.Contains(err.Error(), "write page 2") || r.Written != 2 || r.CommittedPages != 1 || len(s.writes) != 1 {
		t.Fatalf("report %+v error %v", r, err)
	}
}

func TestRunCancellationDiscardsActivePage(t *testing.T) {
	for _, size := range []int{0, 2} {
		ctx, cancel := context.WithCancel(context.Background())
		s := fixtureStore(4)
		r, err := quiet(t.TempDir()).processRun(ctx, s, resolveFunc(func(ctx context.Context, p geocode.Point) (geocode.Result, error) {
			if p.Lat == 4 {
				cancel()
			} // last call returns a result despite cancellation
			return namedPoint(ctx, p)
		}), runOptions{PageSize: size})
		cancel()
		want := 0
		if size == 2 {
			want = 2
		}
		if !errors.Is(err, context.Canceled) || r.Written != want {
			t.Fatalf("size %d: report %+v error %v", size, r, err)
		}
	}
}

func TestDryRunPageParityAndDebug(t *testing.T) {
	var baseline string
	for _, size := range []int{0, 1, 2, 1000} {
		s := fixtureStore(5)
		a := quiet(t.TempDir())
		var logs, csv strings.Builder
		a.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		r, err := a.processRun(context.Background(), s, resolveFunc(namedPoint), runOptions{PageSize: size, DryRun: true})
		if err != nil || len(s.writes) != 0 || r.Written != 0 || r.CommittedPages != 0 {
			t.Fatalf("report %+v error %v", r, err)
		}
		if err := printDryRun(&csv, r.Updates, nil, "bundled", nil, nil, false, r.Selected); err != nil {
			t.Fatal(err)
		}
		if baseline == "" {
			baseline = csv.String()
		}
		if csv.String() != baseline || strings.Contains(csv.String(), "DEBUG") {
			t.Fatal("CSV changed with page size/logging")
		}
		if strings.Count(logs.String(), "msg=resolved") != 5 || strings.Contains(logs.String(), "page committed") {
			t.Fatalf("log %s", logs.String())
		}
	}
}

func TestDryRunHeaderCounts(t *testing.T) {
	var out strings.Builder
	served, filled := map[string]int{"en": 3, "de": 5}, map[string]int{"state": 2, "country": 7}
	if err := printDryRun(&out, nil, nil, "bundled", served, filled, false, 9); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "# names de=5 en=3\n") || !strings.Contains(got, "# fallback country=7 state=2\n") {
		t.Errorf("header counts: %q", got)
	}
	out.Reset()
	if err := printDryRun(&out, nil, nil, "bundled", nil, nil, false, 0); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "# names\n# fallback\n") {
		t.Errorf("empty counts: %q", got)
	}
}

type failedOutput struct{}

func (failedOutput) Write([]byte) (int, error) { return 0, errors.New("output closed") }

func TestDryRunReportsFinalFlushFailure(t *testing.T) {
	if err := printDryRun(failedOutput{}, nil, nil, "bundled", nil, nil, false, 0); err == nil {
		t.Fatal("small buffered output failure was reported as success")
	}
}

func TestRunEmptySelection(t *testing.T) {
	for _, size := range []int{0, 1000} {
		s := fixtureStore(0)
		r, err := quiet(t.TempDir()).processRun(context.Background(), s, resolveFunc(namedPoint), runOptions{PageSize: size})
		if err != nil || r.Selected != 0 || r.CommittedPages != 0 || len(s.writes) != 0 {
			t.Fatalf("size %d: report %+v error %v", size, r, err)
		}
	}
}

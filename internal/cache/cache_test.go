package cache

import (
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/alexzeitgeist/immich-placenames/internal/geo"
)

func wkbPoint(x, y float64) []byte {
	b := []byte{1}
	b = binary.LittleEndian.AppendUint32(b, 1)
	b = binary.LittleEndian.AppendUint64(b, math.Float64bits(x))
	return binary.LittleEndian.AppendUint64(b, math.Float64bits(y))
}

var rows = []Row{
	{ID: "a", Country: "HR", Name: "Alpha", Primary: "Alfa", Common: map[string]string{"en": "Alpha", "de": "Alpha"}, Subtype: "locality", Class: "land", AdminLevel: -1, Territorial: true, Land: false, Bbox: geo.Bbox{XMin: 1, YMin: 2, XMax: 3, YMax: 4}},
	{ID: "b", Country: "HR", Name: "Beta", Subtype: "county", Class: "land", AdminLevel: 2, Territorial: false, Land: true, Bbox: geo.Bbox{XMin: 10, YMin: 10, XMax: 20, YMax: 20}},
}

func write(t *testing.T, dest string) Header {
	t.Helper()
	hdr := Header{Kind: "divisions", Code: "HR", Release: "2026-08-19.0", Fetched: time.Now().UTC()}
	w, err := NewWriter(dest, hdr)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Add(rows[0], wkbPoint(2, 3)); err != nil {
		t.Fatal(err)
	}
	if err := w.Add(rows[1], wkbPoint(15, 15)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return hdr
}

func TestRoundTrip(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "divisions", "HR.geo")
	hdr := write(t, dest)
	f, err := Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if f.Header.Kind != hdr.Kind || f.Header.Code != hdr.Code || f.Header.Release != hdr.Release || !f.Header.Fetched.Equal(hdr.Fetched) {
		t.Errorf("header %+v", f.Header)
	}
	if len(f.Rows) != 2 {
		t.Fatalf("rows %d", len(f.Rows))
	}
	for i, want := range rows {
		got := f.Rows[i]
		want.Off, want.Len = got.Off, got.Len
		if !reflect.DeepEqual(got, want) {
			t.Errorf("row %d: got %+v want %+v", i, got, want)
		}
	}
	if f.Rows[0].Off != int64(len(magic)) || f.Rows[0].Len != 21 || f.Rows[1].Off != int64(len(magic))+21 {
		t.Errorf("offsets %+v %+v", f.Rows[0], f.Rows[1])
	}
	if !f.HasPrimary || !f.HasCommon || !reflect.DeepEqual(f.Languages(), []string{"de", "en"}) {
		t.Errorf("HasPrimary %t HasCommon %t languages %v", f.HasPrimary, f.HasCommon, f.Languages())
	}
	if c := f.Candidates(15, 15); len(c) != 1 || c[0] != 1 {
		t.Errorf("candidates %v", c)
	}
	g, err := f.Geometry(1)
	if err != nil || !g.Contains(15, 15) {
		t.Errorf("geometry: %v", err)
	}
	if g2, _ := f.Geometry(1); g2 != g {
		t.Error("geometry not kept")
	}
	if left, _ := filepath.Glob(dest + ".tmp-*"); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

func TestTruncatedRejected(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "HR.geo")
	write(t, dest)
	st, _ := os.Stat(dest)
	if err := os.Truncate(dest, st.Size()-10); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dest); !errors.Is(err, ErrInvalid) {
		t.Errorf("truncated file: %v", err)
	}
	if err := os.WriteFile(dest, []byte("not a cache file at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dest); !errors.Is(err, ErrInvalid) {
		t.Errorf("garbage file: %v", err)
	}
	if _, err := Open(filepath.Join(t.TempDir(), "missing.geo")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: %v", err)
	}
}

func TestInterruptedWriteLeavesNoFile(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "HR.geo")
	w, err := NewWriter(dest, Header{Kind: "divisions"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Add(rows[0], wkbPoint(2, 3)); err != nil {
		t.Fatal(err)
	}
	w.Abort()
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("destination exists after abort: %v", err)
	}
	if left, _ := filepath.Glob(dest + ".tmp-*"); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
	if err := w.Close(); err == nil {
		t.Error("close after abort succeeded")
	}
}

func scan(rows []Row, lon, lat float64) []int {
	var out []int
	for i := range rows {
		if rows[i].Bbox.Contains(lon, lat) {
			out = append(out, i)
		}
	}
	return out
}

func TestCandidatesMatchScan(t *testing.T) {
	var rs []Row
	add := func(b geo.Bbox) { rs = append(rs, Row{ID: strconv.Itoa(len(rs)), Bbox: b}) }
	for x := -40.0; x <= 40; x += 3.5 {
		for y := -20.0; y <= 20; y += 2.5 {
			add(geo.Bbox{XMin: x, YMin: y, XMax: x + 1.25, YMax: y + 0.75})
		}
	}
	add(geo.Bbox{XMin: -180, YMin: -90, XMax: 180, YMax: 90}) // covers every cell
	add(geo.Bbox{XMin: 0, YMin: 0, XMax: 0, YMax: 0})         // no area
	add(geo.Bbox{XMin: -40, YMin: -20, XMax: -40, YMax: 20})  // no width
	add(geo.Bbox{XMin: 5, YMin: 5, XMax: 6, YMax: 6})
	add(geo.Bbox{XMin: 5, YMin: 5, XMax: 6, YMax: 6}) // same box twice
	f := &File{Rows: rs}
	g := newGrid(rs)
	if g.nx < 4 {
		t.Fatalf("%d rows in a %dx%d grid: too coarse to test", len(rs), g.nx, g.ny)
	}
	var xs, ys []float64
	for k := range g.nx + 1 {
		// Cell edges, and either side of them.
		e := g.bbox.XMin + float64(k)*g.dx
		xs = append(xs, e, math.Nextafter(e, -200), math.Nextafter(e, 200))
		e = g.bbox.YMin + float64(k)*g.dy
		ys = append(ys, e, math.Nextafter(e, -200), math.Nextafter(e, 200))
	}
	for x := -45.0; x <= 45; x += 1.1 {
		xs = append(xs, x)
	}
	for y := -25.0; y <= 25; y += 1.3 {
		ys = append(ys, y)
	}
	xs = append(xs, -180, 180, math.NaN(), math.Inf(1), math.Inf(-1))
	ys = append(ys, -90, 90, math.NaN(), math.Inf(1), math.Inf(-1))
	for _, x := range xs {
		for _, y := range ys {
			if got, want := f.Candidates(x, y), scan(rs, x, y); !reflect.DeepEqual(got, want) {
				t.Fatalf("(%v,%v): got %v, want %v", x, y, got, want)
			}
		}
	}
	if c := (&File{}).Candidates(0, 0); c != nil {
		t.Errorf("no rows: %v", c)
	}
}

// Including a NaN box in the grid's union would reject every query.
func TestCandidatesIgnoreUnusableBoxes(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	for name, bad := range map[string]geo.Bbox{
		"nan":      {XMin: nan, YMin: nan, XMax: nan, YMax: nan},
		"nan max":  {XMin: 0, YMin: 0, XMax: nan, YMax: 1},
		"inverted": {XMin: 5, YMin: 5, XMax: -5, YMax: -5},
		"infinite": {XMin: -inf, YMin: -inf, XMax: inf, YMax: inf},
	} {
		t.Run(name, func(t *testing.T) {
			rs := []Row{{ID: "bad", Bbox: bad}, {ID: "a", Bbox: geo.Bbox{XMin: 1, YMin: 2, XMax: 3, YMax: 4}}}
			f := &File{Rows: rs}
			for _, p := range [][2]float64{{2, 3}, {1, 2}, {3, 4}, {0, 0}, {9, 9}, {nan, 3}} {
				if got, want := f.Candidates(p[0], p[1]), scan(rs, p[0], p[1]); !reflect.DeepEqual(got, want) {
					t.Errorf("(%v,%v): got %v, want %v", p[0], p[1], got, want)
				}
			}
		})
	}
	if c := (&File{Rows: []Row{{Bbox: geo.Bbox{XMin: math.NaN()}}}}).Candidates(0, 0); c != nil {
		t.Errorf("only an unusable row: %v", c)
	}
}

func TestCandidatesOverlappingBoxes(t *testing.T) {
	for count, wantN := range map[int]int{16: 4, 25: 2, 131072: 4} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			// At 16 rows every row covers 16 cells, exactly the budget.
			// At 25 rows the grid coarsens once; at 131072 it coarsens
			// repeatedly instead of overflowing the old int32 prefix sum.
			rows := make([]Row, count+2)
			for i := range count {
				rows[i].Bbox = geo.Bbox{XMin: 0, YMin: 0, XMax: 1, YMax: 1}
			}
			rows[count].Bbox = geo.Bbox{XMin: math.NaN()}
			rows[count+1].Bbox = geo.Bbox{XMin: 1, XMax: -1}
			f := &File{Rows: rows}
			for _, p := range [][2]float64{{0.5, 0.5}, {0, 0}, {1, 1}, {2, 0.5}, {math.NaN(), 0.5}, {math.Inf(1), 0.5}} {
				if got, want := f.Candidates(p[0], p[1]), scan(rows, p[0], p[1]); !reflect.DeepEqual(got, want) {
					t.Fatalf("(%v,%v): candidates differ from scan (lengths %d and %d)", p[0], p[1], len(got), len(want))
				}
			}
			if f.grid == nil {
				t.Fatal("no grid for overlapping boxes")
			}
			if f.grid.nx != wantN || f.grid.ny != wantN {
				t.Fatalf("grid = %dx%d, want %dx%d", f.grid.nx, f.grid.ny, wantN, wantN)
			}
			if len(f.grid.rows) > min(count*gridEntriesPerRow, gridMaxEntries) {
				t.Fatalf("%d entries exceed the budget", len(f.grid.rows))
			}
		})
	}
}

func TestCandidatesConcurrentFirstUse(t *testing.T) {
	var rs []Row
	for i := range 256 {
		x := float64(i)
		rs = append(rs, Row{Bbox: geo.Bbox{XMin: x, YMin: 0, XMax: x + 2, YMax: 2}})
	}
	f := &File{Rows: rs}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			for j := range 32 {
				x := float64(i*16+j) - 1
				if got, want := f.Candidates(x, 1), scan(rs, x, 1); !reflect.DeepEqual(got, want) {
					t.Errorf("(%g,1): got %v, want %v", x, got, want)
					return
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()
}

func TestCandidatesGridBudget(t *testing.T) {
	rows := make([]Row, 64)
	for i := range rows {
		// Mix broad and small boxes to test coarsening, filtering and row order.
		rows[i].Bbox = geo.Bbox{XMin: 0, YMin: 0, XMax: 4, YMax: 4}
		if i%2 != 0 {
			x, y := float64(i%4), float64((i/4)%4)
			rows[i].Bbox = geo.Bbox{XMin: x, YMin: y, XMax: x + 0.5, YMax: y + 0.5}
		}
	}
	for _, tc := range []struct {
		name          string
		budget, wantN int
	}{
		{"coarsen", 1024, 4},
		{"one cell", 64, 1},
		{"cannot fit one cell", 63, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &File{Rows: rows}
			f.once.Do(func() { f.grid = newGridWithBudget(rows, tc.budget) })
			if tc.wantN == 0 {
				if f.grid != nil {
					t.Fatal("expected scan fallback")
				}
			} else if f.grid == nil || f.grid.nx != tc.wantN || f.grid.ny != tc.wantN || len(f.grid.rows) > tc.budget {
				t.Fatalf("grid does not meet dimension %d and budget %d", tc.wantN, tc.budget)
			}
			coords := []float64{-1, 0, 0.25, 0.5, 1, 2, 3, 4, 5, math.NaN(), math.Inf(-1), math.Inf(1)}
			for _, edge := range []float64{0, 1, 2, 3, 4} {
				coords = append(coords, math.Nextafter(edge, math.Inf(-1)), math.Nextafter(edge, math.Inf(1)))
			}
			for _, x := range coords {
				for _, y := range coords {
					if got, want := f.Candidates(x, y), scan(rows, x, y); !reflect.DeepEqual(got, want) {
						t.Fatalf("(%v,%v): got %v, want %v", x, y, got, want)
					}
				}
			}
		})
	}
}

// Writers of one destination in one process get their own temporary files;
// the last rename wins with a complete file. All four hold their files open
// before any writes, so a shared name would collide.
func TestConcurrentWritersSameDest(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "HR.geo")
	const rowsEach = 200
	var wg, opened sync.WaitGroup
	opened.Add(4)
	start := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w, err := NewWriter(dest, Header{Kind: "divisions", Release: strconv.Itoa(i)})
			opened.Done()
			if err != nil {
				t.Error(err)
				return
			}
			<-start
			for j := 0; j < rowsEach; j++ {
				if err := w.Add(rows[0], wkbPoint(float64(i), float64(j))); err != nil {
					t.Error(err)
				}
			}
			if err := w.Close(); err != nil {
				t.Error(err)
			}
		}(i)
	}
	opened.Wait()
	close(start)
	wg.Wait()
	f, err := Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	i, _ := strconv.Atoi(f.Header.Release)
	if len(f.Rows) != rowsEach {
		t.Fatalf("rows %d", len(f.Rows))
	}
	for j := range f.Rows {
		if g, err := f.Geometry(j); err != nil || !g.Contains(float64(i), float64(j)) {
			t.Fatalf("row %d of writer %d: %v", j, i, err)
		}
	}
	if st, _ := os.Stat(dest); st.Mode().Perm() != 0o644 {
		t.Errorf("mode %o", st.Mode().Perm())
	}
	if left, _ := filepath.Glob(dest + ".tmp-*"); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

func TestEmptyWriterKeepsPreviousFile(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "divisions", "HR.geo")
	write(t, dest)
	w, err := NewWriter(dest, Header{Kind: "divisions", Code: "HR", Release: "2026-09-16.0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); !errors.Is(err, ErrNoRows) {
		t.Fatalf("Close of an empty writer: %v, want ErrNoRows", err)
	}
	f, err := Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if f.Header.Release != "2026-08-19.0" || len(f.Rows) != len(rows) {
		t.Errorf("previous file replaced: header %+v, %d rows", f.Header, len(f.Rows))
	}
	if left, _ := filepath.Glob(dest + ".tmp-*"); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

func TestHeaderDependencies(t *testing.T) {
	for _, want := range []bool{false, true} {
		dest := filepath.Join(t.TempDir(), "world.geo")
		w, err := NewWriter(dest, Header{Kind: "world", Release: "2026-08-19.0", Dependencies: want})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Add(rows[0], wkbPoint(2, 3)); err != nil {
			w.Abort()
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		f, err := Open(dest)
		if err != nil {
			t.Fatal(err)
		}
		if f.Header.Dependencies != want {
			t.Errorf("Dependencies %v, want %v", f.Header.Dependencies, want)
		}
		f.Close()
	}
}

func TestNear(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "divisions", "HR.geo")
	write(t, dest)
	c, err := Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, tc := range []struct {
		lon, lat, d float64
		want        []int
	}{
		{lon: 2, lat: 3, d: 0, want: []int{0}},
		{lon: 0, lat: 3, d: 0.9},
		{lon: 0, lat: 3, d: 1, want: []int{0}},
		{lon: 5, lat: 5, d: 10, want: []int{0, 1}},
	} {
		if got := c.Near(tc.lon, tc.lat, tc.d); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Near(%g, %g, %g) = %v, want %v", tc.lon, tc.lat, tc.d, got, tc.want)
		}
	}
}

func TestNearWrappedSquare(t *testing.T) {
	for _, tc := range []struct {
		name        string
		lon, lat, d float64
		box         geo.Bbox
		want        []int
	}{
		{"east wrap", 179.999, -16, 0.005, geo.Bbox{XMin: -179.999, XMax: -179.999, YMin: -16, YMax: -16}, []int{0}},
		{"west wrap", -179.999, -16, 0.005, geo.Bbox{XMin: 179.999, XMax: 179.999, YMin: -16, YMax: -16}, []int{0}},
		{"square corner", 0, 0, 1, geo.Bbox{XMin: 0.9, XMax: 0.9, YMin: 0.9, YMax: 0.9}, []int{0}},
		{"opposite dateline endpoints", 180, 0, 0, geo.Bbox{XMin: -180, XMax: -180}, []int{0}},
		{"latitude outside", 179.999, -16, 0.005, geo.Bbox{XMin: -179.999, XMax: -179.999, YMin: -15, YMax: -15}, nil},
		{"longitude outside", 179.999, -16, 0.005, geo.Bbox{XMin: -179.9, XMax: -179.9, YMin: -16, YMax: -16}, nil},
		{"all longitudes", 170, 90, 180, geo.Bbox{XMin: -170, XMax: -170, YMin: 89, YMax: 89}, []int{0}},
		{"no duplicate rows", 180, 0, 180, geo.Bbox{XMin: -180, XMax: 180, YMin: -90, YMax: 90}, []int{0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &File{Rows: []Row{{Bbox: tc.box}}}
			if got := c.Near(tc.lon, tc.lat, tc.d); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Near(%g, %g, %g) = %v, want %v", tc.lon, tc.lat, tc.d, got, tc.want)
			}
		})
	}
}

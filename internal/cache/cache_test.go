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

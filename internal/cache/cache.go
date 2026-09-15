// Package cache stores fetched Overture rows as one file per kind and country:
// WKB blobs back to back, then a gob-encoded header and index, then the
// index offset. Writers publish by rename, readers validate before use.
package cache

import (
	"bufio"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/alexzeitgeist/immich-placenames/internal/geo"
)

const magic = "RGEO0001"

// ErrInvalid marks a file that failed validation; callers treat it as absent.
var ErrInvalid = errors.New("invalid cache file")

// ErrNoRows means the writer had no rows and left the destination untouched.
var ErrNoRows = errors.New("no rows to cache")

// Header describes what a file holds.
type Header struct {
	Kind    string // world, divisions, airports
	Code    string // alpha-2, empty for world
	Release string
	Fetched time.Time
	// Dependencies marks world caches built with dependency territories.
	// Older files decode it as false.
	Dependencies bool
}

// Row is one geometry's index entry.
type Row struct {
	ID          string
	Country     string
	Name        string
	Primary     string            // Overture's local name; empty in files written before it was stored
	Common      map[string]string // translations by language code; nil in files written before they were stored
	Subtype     string
	Class       string
	AdminLevel  int32 // -1 when unknown
	Territorial bool
	Land        bool
	Bbox        geo.Bbox
	Off         int64
	Len         int32
}

type tail struct {
	Header Header
	Rows   []Row
}

// Writer streams blobs into a temporary file and publishes it on Close.
type Writer struct {
	dest, tmp string
	f         *os.File
	w         *bufio.Writer
	off       int64
	hdr       Header
	rows      []Row
	done      bool
}

// NewWriter opens a uniquely named dest.tmp-* beside dest, creating the
// directory; writers of one destination never share it.
func NewWriter(dest string, hdr Header) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".tmp-*")
	if err != nil {
		return nil, err
	}
	w := &Writer{dest: dest, tmp: f.Name(), f: f, w: bufio.NewWriterSize(f, 1<<20), hdr: hdr}
	if err := f.Chmod(0o644); err != nil {
		w.Abort()
		return nil, err
	}
	if _, err := w.w.WriteString(magic); err != nil {
		w.Abort()
		return nil, err
	}
	w.off = int64(len(magic))
	return w, nil
}

// Add appends one blob and its index row.
func (w *Writer) Add(r Row, wkb []byte) error {
	r.Off, r.Len = w.off, int32(len(wkb))
	if _, err := w.w.Write(wkb); err != nil {
		return err
	}
	w.off += int64(len(wkb))
	w.rows = append(w.rows, r)
	return nil
}

// Count is the number of rows added.
func (w *Writer) Count() int { return len(w.rows) }

// Close writes the index and trailer, syncs and renames over dest. A writer
// that added no rows publishes nothing and returns ErrNoRows.
func (w *Writer) Close() error {
	if w.done {
		return errors.New("cache writer already closed")
	}
	if len(w.rows) == 0 {
		w.Abort()
		return ErrNoRows
	}
	err := func() error {
		if err := gob.NewEncoder(w.w).Encode(tail{w.hdr, w.rows}); err != nil {
			return err
		}
		var tb [8]byte
		binary.LittleEndian.PutUint64(tb[:], uint64(w.off))
		if _, err := w.w.Write(tb[:]); err != nil {
			return err
		}
		if err := w.w.Flush(); err != nil {
			return err
		}
		if err := w.f.Sync(); err != nil {
			return err
		}
		if err := w.f.Close(); err != nil {
			return err
		}
		return os.Rename(w.tmp, w.dest)
	}()
	if err != nil {
		w.Abort()
		return err
	}
	w.done = true
	if d, err := os.Open(filepath.Dir(w.dest)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// Abort discards the temporary file; dest is untouched.
func (w *Writer) Abort() {
	if w.done {
		return
	}
	w.done = true
	w.f.Close()
	os.Remove(w.tmp)
}

// File is an opened cache: the index in memory, geometries decoded on first
// use. HasPrimary and HasCommon report whether any row carries a local name
// or translations.
type File struct {
	Path       string
	Header     Header
	Rows       []Row
	HasPrimary bool
	HasCommon  bool
	f          *os.File
	size       int64
	mu         sync.Mutex
	geoms      []*geo.Geometry
}

func invalid(path string, msg string) error {
	return fmt.Errorf("%s: %s: %w", path, msg, ErrInvalid)
}

// Open reads and validates the index. A missing file returns an fs.ErrNotExist error.
func Open(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	c, err := open(path, f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return c, nil
}

func open(path string, f *os.File) (*File, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	if size < int64(len(magic))+8 {
		return nil, invalid(path, "too short")
	}
	var m [len(magic)]byte
	if _, err := f.ReadAt(m[:], 0); err != nil {
		return nil, invalid(path, err.Error())
	}
	if string(m[:]) != magic {
		return nil, invalid(path, "bad magic")
	}
	var tb [8]byte
	if _, err := f.ReadAt(tb[:], size-8); err != nil {
		return nil, invalid(path, err.Error())
	}
	tailOff := int64(binary.LittleEndian.Uint64(tb[:]))
	if tailOff < int64(len(magic)) || tailOff > size-8 {
		return nil, invalid(path, "index offset outside file")
	}
	var t tail
	sec := io.NewSectionReader(f, tailOff, size-8-tailOff)
	if err := gob.NewDecoder(bufio.NewReader(sec)).Decode(&t); err != nil {
		return nil, invalid(path, "index: "+err.Error())
	}
	primary, common := false, false
	for i, r := range t.Rows {
		if r.Off < int64(len(magic)) || r.Len < 0 || r.Off+int64(r.Len) > tailOff {
			return nil, invalid(path, fmt.Sprintf("row %d blob outside blob region", i))
		}
		primary = primary || r.Primary != ""
		common = common || len(r.Common) > 0
	}
	return &File{Path: path, Header: t.Header, Rows: t.Rows, HasPrimary: primary, HasCommon: common, f: f, size: size, geoms: make([]*geo.Geometry, len(t.Rows))}, nil
}

// Languages lists the translation codes present in any row, sorted.
func (c *File) Languages() []string {
	set := map[string]bool{}
	for i := range c.Rows {
		for k := range c.Rows[i].Common {
			set[k] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Size is the file size in bytes.
func (c *File) Size() int64 { return c.size }

// Close releases the file; decoded geometries stay valid.
func (c *File) Close() error { return c.f.Close() }

// Candidates returns the indices of rows whose bbox contains the point.
func (c *File) Candidates(lon, lat float64) []int {
	var out []int
	for i := range c.Rows {
		if c.Rows[i].Bbox.Contains(lon, lat) {
			out = append(out, i)
		}
	}
	return out
}

// Near returns rows whose bbox intersects a square of half-width d degrees
// around the point, wrapping longitude. Callers must check geometry distances.
func (c *File) Near(lon, lat, d float64) []int {
	var out []int
	box := geo.Bbox{XMin: lon - d, YMin: lat - d, XMax: lon + d, YMax: lat + d}
	west, east := box, box
	west.XMin, west.XMax = box.XMin-360, box.XMax-360
	east.XMin, east.XMax = box.XMin+360, box.XMax+360
	for i := range c.Rows {
		b := c.Rows[i].Bbox
		if b.Intersects(box) || b.Intersects(west) || b.Intersects(east) {
			out = append(out, i)
		}
	}
	return out
}

// Geometry decodes row i's blob on first use and keeps it.
func (c *File) Geometry(i int) (*geo.Geometry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if g := c.geoms[i]; g != nil {
		return g, nil
	}
	r := c.Rows[i]
	buf := make([]byte, r.Len)
	if _, err := c.f.ReadAt(buf, r.Off); err != nil {
		return nil, fmt.Errorf("%s row %d: %w", c.Path, i, err)
	}
	g, err := geo.Decode(buf)
	if err != nil {
		return nil, fmt.Errorf("%s row %d (%s): %w", c.Path, i, r.ID, err)
	}
	c.geoms[i] = g
	return g, nil
}

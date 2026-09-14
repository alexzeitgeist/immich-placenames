// Package fetch builds cache files from Overture's Parquet releases over HTTP
// range requests: footers, row-group statistics and the small filter column
// decide which row groups to read; each admitted group costs one request.
package fetch

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/format"

	"github.com/alexzeitgeist/immich-placenames/internal/cache"
	"github.com/alexzeitgeist/immich-placenames/internal/geo"
)

const (
	// CatalogURL names the latest release in its "latest" field.
	CatalogURL = "https://stac.overturemaps.org/catalog.json"
	// Bucket serves the release files anonymously with range requests.
	Bucket          = "https://overturemaps-us-west-2.s3.us-west-2.amazonaws.com/"
	divisionsPrefix = "theme=divisions/type=division_area/"
	infraPrefix     = "theme=base/type=infrastructure/"
	footerBytes     = 4 << 20
	batchRows       = 16
)

// Client fetches with Workers parallel row-group readers.
type Client struct {
	HTTP    *http.Client
	Workers int
	Log     *slog.Logger

	once     sync.Once
	requests atomic.Int64
	bytes    atomic.Int64
}

// client initializes the HTTP client once, unless one was injected.
// Concurrent readers share it. It clones the standard default transport
// or creates a fresh transport if the default was replaced.
func (c *Client) client() *http.Client {
	c.once.Do(func() {
		if c.HTTP != nil {
			return
		}
		t, ok := http.DefaultTransport.(*http.Transport)
		if ok {
			t = t.Clone()
		} else {
			t = &http.Transport{Proxy: http.ProxyFromEnvironment}
		}
		t.ResponseHeaderTimeout = 60 * time.Second
		// Bound the whole request, including a body that stalls after headers.
		c.HTTP = &http.Client{Transport: t, Timeout: 2 * time.Minute}
	})
	return c.HTTP
}

func (c *Client) workers() int {
	if c.Workers > 0 {
		return c.Workers
	}
	return 4
}

func (c *Client) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

// LatestRelease reads the release name from the STAC catalog.
func (c *Client) LatestRelease(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, CatalogURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("catalog: %s", resp.Status)
	}
	var cat struct {
		Latest string `json:"latest"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cat); err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	if cat.Latest == "" {
		return "", fmt.Errorf("catalog: no latest release")
	}
	return cat.Latest, nil
}

// World writes every subtype=country row into dest.
func (c *Client) World(ctx context.Context, release, dest string) (int, error) {
	hdr := cache.Header{Kind: "world", Release: release, Fetched: time.Now().UTC()}
	return c.divisions(ctx, release, "subtype", "country", nil, hdr, dest, func(r *record) bool {
		return r.subtype == "country"
	})
}

// Divisions writes every division_area row of the country into dest,
// admitting row groups by the country's bbox.
func (c *Client) Divisions(ctx context.Context, release, code string, box geo.Bbox, dest string) (int, error) {
	hdr := cache.Header{Kind: "divisions", Code: code, Release: release, Fetched: time.Now().UTC()}
	return c.divisions(ctx, release, "country", code, &box, hdr, dest, func(r *record) bool {
		return r.country == code
	})
}

// Airports writes every airport in the release into dest. The rows carry no
// country, and a country's bbox can span the globe, so the file is global.
func (c *Client) Airports(ctx context.Context, release, dest string) (int, error) {
	hdr := cache.Header{Kind: "airports", Release: release, Fetched: time.Now().UTC()}
	refs, err := c.plan(ctx, release, infraPrefix, "subtype", "airport", nil)
	if err != nil {
		return 0, err
	}
	return c.build(ctx, refs, hdr, dest, func(ref groupRef, add func(cache.Row, []byte) error) error {
		return readGroup(ctx, ref, func(r *record) error {
			if !r.complete() || !r.isAirport() {
				return nil
			}
			return add(cache.Row{
				ID: r.id, Name: r.name, Primary: r.primary, Common: r.common(), Subtype: r.subtype, Class: r.class,
				AdminLevel: -1, Land: true, Bbox: r.bbox,
			}, r.geometry)
		})
	})
}

func (c *Client) divisions(ctx context.Context, release, filterCol, filterVal string, box *geo.Bbox, hdr cache.Header, dest string, keep func(*record) bool) (int, error) {
	refs, err := c.plan(ctx, release, divisionsPrefix, filterCol, filterVal, box)
	if err != nil {
		return 0, err
	}
	return c.build(ctx, refs, hdr, dest, func(ref groupRef, add func(cache.Row, []byte) error) error {
		return readGroup(ctx, ref, func(r *record) error {
			if !keep(r) || !r.complete() {
				return nil
			}
			return add(cache.Row{
				ID: r.id, Country: r.country, Name: r.name, Primary: r.primary, Common: r.common(), Subtype: r.subtype, Class: r.class,
				AdminLevel: r.adminLevel, Land: r.isLand, Territorial: r.isTerritorial, Bbox: r.bbox,
			}, r.geometry)
		})
	})
}

// build reads the admitted groups in parallel into one cache file.
func (c *Client) build(ctx context.Context, refs []groupRef, hdr cache.Header, dest string, read func(groupRef, func(cache.Row, []byte) error) error) (int, error) {
	start := time.Now()
	c.requests.Store(0)
	c.bytes.Store(0)
	w, err := cache.NewWriter(dest, hdr)
	if err != nil {
		return 0, err
	}
	var mu sync.Mutex
	add := func(r cache.Row, wkb []byte) error {
		mu.Lock()
		defer mu.Unlock()
		return w.Add(r, wkb)
	}
	var done atomic.Int64
	err = c.each(ctx, refs, func(ref groupRef) error {
		if err := read(ref, add); err != nil {
			return fmt.Errorf("%s row group %d: %w", ref.obj.Key, ref.index, err)
		}
		mu.Lock()
		count := w.Count()
		mu.Unlock()
		c.log().Debug("row group read", "done", done.Add(1), "of", len(refs), "rows", count)
		return nil
	})
	if err != nil {
		w.Abort()
		return 0, err
	}
	if w.Count() == 0 {
		w.Abort()
		return 0, fmt.Errorf("%s %s: no rows in release %s; previous file kept", hdr.Kind, hdr.Code, hdr.Release)
	}
	if err := w.Close(); err != nil {
		return 0, err
	}
	c.log().Info("cache written", "path", dest, "rows", w.Count(), "mb", c.bytes.Load()/1e6, "requests", c.requests.Load(), "took", time.Since(start).Round(time.Second))
	return w.Count(), nil
}

type object struct {
	Key  string
	Size int64
}

func (c *Client) list(ctx context.Context, prefix string) ([]object, error) {
	var out []object
	token := ""
	for {
		u := Bucket + "?list-type=2&prefix=" + url.QueryEscape(prefix)
		if token != "" {
			u += "&continuation-token=" + url.QueryEscape(token)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.client().Do(req)
		if err != nil {
			return nil, err
		}
		var lr struct {
			Contents []struct {
				Key  string `xml:"Key"`
				Size int64  `xml:"Size"`
			} `xml:"Contents"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("list %s: %s", prefix, resp.Status)
		}
		err = xml.NewDecoder(resp.Body).Decode(&lr)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", prefix, err)
		}
		for _, o := range lr.Contents {
			if strings.HasSuffix(o.Key, ".parquet") {
				out = append(out, object{o.Key, o.Size})
			}
		}
		if !lr.IsTruncated {
			break
		}
		token = lr.NextContinuationToken
	}
	slices.SortFunc(out, func(a, b object) int { return strings.Compare(a.Key, b.Key) })
	if len(out) == 0 {
		return nil, fmt.Errorf("list %s: no parquet files", prefix)
	}
	return out, nil
}

// span is a prefetched byte range served to parquet-go's ReadAt calls.
type span struct {
	off  int64
	data []byte
}

// rangeReader is an io.ReaderAt over one object: prefetched spans first, HTTP ranges otherwise.
type rangeReader struct {
	c     *Client
	ctx   context.Context
	url   string
	mu    sync.Mutex
	spans []*span
}

func (r *rangeReader) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	for _, s := range r.spans {
		if off >= s.off && off+int64(len(p)) <= s.off+int64(len(s.data)) {
			n := copy(p, s.data[off-s.off:])
			r.mu.Unlock()
			return n, nil
		}
	}
	r.mu.Unlock()
	return r.fetch(p, off)
}

func (r *rangeReader) prefetch(off, n int64) (*span, error) {
	buf := make([]byte, n)
	m, err := r.fetch(buf, off)
	if err != nil && err != io.EOF {
		return nil, err
	}
	s := &span{off: off, data: buf[:m]}
	r.mu.Lock()
	r.spans = append(r.spans, s)
	r.mu.Unlock()
	return s, nil
}

func (r *rangeReader) release(s *span) {
	r.mu.Lock()
	r.spans = slices.DeleteFunc(r.spans, func(x *span) bool { return x == s })
	r.mu.Unlock()
}

func (r *rangeReader) fetch(p []byte, off int64) (int, error) {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			case <-r.ctx.Done():
				return 0, r.ctx.Err()
			}
		}
		n, err := r.once(p, off)
		if err == nil || err == io.EOF {
			return n, err
		}
		if r.ctx.Err() != nil {
			return 0, r.ctx.Err()
		}
		last = err
		r.c.log().Warn("range request failed, retrying", "url", r.url, "off", off, "len", len(p), "err", err)
	}
	return 0, last
}

func (r *rangeReader) once(p []byte, off int64) (int, error) {
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+int64(len(p))-1))
	resp, err := r.c.client().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	r.c.requests.Add(1)
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("range %d+%d: %s", off, len(p), resp.Status)
	}
	n, err := io.ReadFull(resp.Body, p)
	r.c.bytes.Add(int64(n))
	if err == io.ErrUnexpectedEOF {
		err = io.EOF
	}
	return n, err
}

// groupRef is one admitted row group.
type groupRef struct {
	obj   object
	pf    *parquet.File
	rr    *rangeReader
	cols  columns
	index int
}

// plan lists the files, reads their footers and admits row groups: bbox
// statistics overlap, then the filter column's min and max, then its values.
func (c *Client) plan(ctx context.Context, release, prefix, filterCol, filterVal string, box *geo.Bbox) ([]groupRef, error) {
	start := time.Now()
	c.requests.Store(0)
	c.bytes.Store(0)
	objs, err := c.list(ctx, "release/"+release+"/"+prefix)
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	var refs []groupRef
	var groups, rows int64
	err = each(ctx, c.workers(), objs, func(obj object) error {
		rr := &rangeReader{c: c, ctx: ctx, url: Bucket + obj.Key}
		n := min(obj.Size, int64(footerBytes))
		s, err := rr.prefetch(obj.Size-n, n)
		if err != nil {
			return fmt.Errorf("%s: %w", obj.Key, err)
		}
		pf, err := parquet.OpenFile(rr, obj.Size, parquet.SkipPageIndex(true), parquet.SkipBloomFilters(true))
		rr.release(s)
		if err != nil {
			return fmt.Errorf("%s: %w", obj.Key, err)
		}
		admitted, err := c.admit(pf, rr, filterCol, filterVal, box)
		if err != nil {
			return fmt.Errorf("%s: %w", obj.Key, err)
		}
		cols, err := resolveColumns(pf)
		if err != nil {
			return fmt.Errorf("%s: %w", obj.Key, err)
		}
		mu.Lock()
		defer mu.Unlock()
		groups += int64(len(pf.RowGroups()))
		rows += pf.NumRows()
		for _, gi := range admitted {
			refs = append(refs, groupRef{obj, pf, rr, cols, gi})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(refs, func(a, b groupRef) int {
		if a.obj.Key != b.obj.Key {
			return strings.Compare(a.obj.Key, b.obj.Key)
		}
		return a.index - b.index
	})
	var admittedBytes int64
	for _, r := range refs {
		_, n := groupSpan(&r.pf.Metadata().RowGroups[r.index])
		admittedBytes += n
	}
	c.log().Info("row groups admitted", "prefix", prefix, "filter", filterCol+"="+filterVal, "files", len(objs), "groups", groups, "rows", rows,
		"admitted", len(refs), "mb", admittedBytes/1e6, "probe_mb", c.bytes.Load()/1e6, "requests", c.requests.Load(), "took", time.Since(start).Round(time.Second))
	return refs, nil
}

func (c *Client) admit(pf *parquet.File, rr *rangeReader, filterCol, filterVal string, box *geo.Bbox) ([]int, error) {
	leaves := pf.Schema().Columns()
	index := func(path string) int {
		for i, p := range leaves {
			if strings.Join(p, ".") == path {
				return i
			}
		}
		return -1
	}
	fi := index(filterCol)
	if fi < 0 {
		return nil, fmt.Errorf("column %s missing", filterCol)
	}
	xi, yi, xa, ya := index("bbox.xmin"), index("bbox.ymin"), index("bbox.xmax"), index("bbox.ymax")
	if box != nil && (xi < 0 || yi < 0 || xa < 0 || ya < 0) {
		return nil, fmt.Errorf("bbox columns missing")
	}
	md := pf.Metadata()
	rgs := pf.RowGroups()
	var out []int
	for gi := range md.RowGroups {
		rg := &md.RowGroups[gi]
		if box != nil {
			g := geo.Bbox{
				XMin: statFloat(minStat(&rg.Columns[xi].MetaData.Statistics)),
				YMin: statFloat(minStat(&rg.Columns[yi].MetaData.Statistics)),
				XMax: statFloat(maxStat(&rg.Columns[xa].MetaData.Statistics)),
				YMax: statFloat(maxStat(&rg.Columns[ya].MetaData.Statistics)),
			}
			if !math.IsNaN(g.XMin+g.YMin+g.XMax+g.YMax) && !g.Intersects(*box) {
				continue
			}
		}
		st := &rg.Columns[fi].MetaData.Statistics
		if lo, hi := string(minStat(st)), string(maxStat(st)); lo != "" && (filterVal < lo || filterVal > hi) {
			continue
		}
		vals, err := readStrings(rr, rgs[gi].ColumnChunks()[fi], &rg.Columns[fi].MetaData)
		if err != nil {
			return nil, fmt.Errorf("row group %d %s: %w", gi, filterCol, err)
		}
		if slices.Contains(vals, filterVal) {
			out = append(out, gi)
		}
	}
	return out, nil
}

func minStat(s *format.Statistics) []byte {
	if len(s.MinValue) > 0 {
		return s.MinValue
	}
	return s.Min
}

func maxStat(s *format.Statistics) []byte {
	if len(s.MaxValue) > 0 {
		return s.MaxValue
	}
	return s.Max
}

func statFloat(b []byte) float64 {
	switch len(b) {
	case 4:
		return float64(math.Float32frombits(binary.LittleEndian.Uint32(b)))
	case 8:
		return math.Float64frombits(binary.LittleEndian.Uint64(b))
	}
	return math.NaN()
}

// chunkSpan is the byte range of one column chunk: dictionary page first when present.
func chunkSpan(m *format.ColumnMetaData) (off, n int64) {
	off = m.DataPageOffset
	if m.DictionaryPageOffset > 0 && m.DictionaryPageOffset < off {
		off = m.DictionaryPageOffset
	}
	return off, m.TotalCompressedSize
}

// groupSpan is the byte range covering every column chunk of a row group.
func groupSpan(rg *format.RowGroup) (off, n int64) {
	off, end := int64(math.MaxInt64), int64(0)
	for i := range rg.Columns {
		o, l := chunkSpan(&rg.Columns[i].MetaData)
		off = min(off, o)
		end = max(end, o+l)
	}
	return off, end - off
}

// readStrings returns one string per row of the column chunk, "" for null.
func readStrings(rr *rangeReader, chunk parquet.ColumnChunk, meta *format.ColumnMetaData) ([]string, error) {
	off, n := chunkSpan(meta)
	s, err := rr.prefetch(off, n)
	if err != nil {
		return nil, err
	}
	defer rr.release(s)
	pages := chunk.Pages()
	defer pages.Close()
	var out []string
	buf := make([]parquet.Value, 1024)
	for {
		page, err := pages.ReadPage()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		vr := page.Values()
		for {
			n, err := vr.ReadValues(buf)
			for _, v := range buf[:n] {
				if v.IsNull() {
					out = append(out, "")
				} else {
					out = append(out, v.String())
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				parquet.Release(page)
				return nil, err
			}
		}
		parquet.Release(page)
	}
}

// readGroup prefetches one row group and streams its rows to sink as raw
// column values keyed by leaf index, so optional groups cannot shift values
// between rows. A record is valid only during the sink call.
func readGroup(ctx context.Context, ref groupRef, sink func(*record) error) error {
	off, n := groupSpan(&ref.pf.Metadata().RowGroups[ref.index])
	s, err := ref.rr.prefetch(off, n)
	if err != nil {
		return err
	}
	defer ref.rr.release(s)
	rows := ref.pf.RowGroups()[ref.index].Rows()
	defer rows.Close()
	buf := make([]parquet.Row, batchRows)
	var rec record
	for {
		n, err := rows.ReadRows(buf)
		for i := range buf[:n] {
			ref.cols.decode(buf[i], &rec)
			if err := sink(&rec); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// each runs fn over items with bounded parallelism, stopping at the first error.
func each[T any](ctx context.Context, workers int, items []T, fn func(T) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	sem := make(chan struct{}, workers)
	for _, it := range items {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(it T) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(it); err != nil {
				mu.Lock()
				if first == nil {
					first = err
					cancel()
				}
				mu.Unlock()
			}
		}(it)
	}
	wg.Wait()
	if first != nil {
		return first
	}
	return ctx.Err()
}

func (c *Client) each(ctx context.Context, refs []groupRef, fn func(groupRef) error) error {
	return each(ctx, c.workers(), refs, fn)
}

// columns holds the leaf column indexes of one file, -1 when absent.
type columns struct {
	id, geometry, country, subtype, class, adminLevel, isLand, isTerritorial int
	primary, commonKey, commonValue                                          int
	xmin, ymin, xmax, ymax                                                   int
}

func resolveColumns(pf *parquet.File) (columns, error) {
	paths := make([]string, len(pf.Schema().Columns()))
	for i, p := range pf.Schema().Columns() {
		paths[i] = strings.Join(p, ".")
	}
	idx := func(path string) int { return slices.Index(paths, path) }
	find := func(prefix, suffix string) int {
		for i, p := range paths {
			if strings.HasPrefix(p, prefix) && strings.HasSuffix(p, suffix) {
				return i
			}
		}
		return -1
	}
	c := columns{
		id: idx("id"), geometry: idx("geometry"), country: idx("country"), subtype: idx("subtype"), class: idx("class"),
		adminLevel: idx("admin_level"), isLand: idx("is_land"), isTerritorial: idx("is_territorial"),
		primary: idx("names.primary"), commonKey: find("names.common.", ".key"), commonValue: find("names.common.", ".value"),
		xmin: idx("bbox.xmin"), ymin: idx("bbox.ymin"), xmax: idx("bbox.xmax"), ymax: idx("bbox.ymax"),
	}
	if c.id < 0 || c.geometry < 0 || c.subtype < 0 || c.primary < 0 || c.commonKey < 0 || c.commonValue < 0 || c.xmin < 0 || c.ymin < 0 || c.xmax < 0 || c.ymax < 0 {
		return c, fmt.Errorf("columns missing; have %s", strings.Join(paths, " "))
	}
	return c, nil
}

// record is one decoded row. Missing is_land reads true, missing
// is_territorial false, missing admin_level -1.
type record struct {
	id, country, subtype, class, name string
	adminLevel                        int32
	isLand, isTerritorial             bool
	geometry                          []byte
	bbox                              geo.Bbox
	bboxSet                           int
	primary                           string
	keys, values                      []string
}

// common is the row's translations, nil when it has none.
func (r *record) common() map[string]string {
	var m map[string]string
	for i, k := range r.keys {
		if k == "" || i >= len(r.values) || r.values[i] == "" {
			continue
		}
		if m == nil {
			m = make(map[string]string, len(r.keys))
		}
		m[k] = r.values[i]
	}
	return m
}

// complete reports whether the row carries its bbox and geometry.
func (r *record) complete() bool {
	return r.bboxSet == 4 && len(r.geometry) > 0
}

// isAirport reports whether the row is an airport: subtype airport, class
// airport or ending in _airport.
func (r *record) isAirport() bool {
	return r.subtype == "airport" && (r.class == "airport" || strings.HasSuffix(r.class, "_airport"))
}

func valueString(v parquet.Value) string {
	if v.IsNull() {
		return ""
	}
	return v.String()
}

func valueFloat(v parquet.Value) float64 {
	if v.Kind() == parquet.Float {
		return float64(v.Float())
	}
	return v.Double()
}

func (c *columns) decode(row parquet.Row, r *record) {
	*r = record{adminLevel: -1, isLand: true, keys: r.keys[:0], values: r.values[:0]}
	for _, v := range row {
		col := v.Column()
		if col == c.commonKey {
			r.keys = append(r.keys, valueString(v))
			continue
		}
		if col == c.commonValue {
			r.values = append(r.values, valueString(v))
			continue
		}
		if v.IsNull() {
			continue
		}
		switch col {
		case c.id:
			r.id = v.String()
		case c.geometry:
			r.geometry = v.ByteArray()
		case c.country:
			r.country = v.String()
		case c.subtype:
			r.subtype = v.String()
		case c.class:
			r.class = v.String()
		case c.adminLevel:
			r.adminLevel = v.Int32()
		case c.isLand:
			r.isLand = v.Boolean()
		case c.isTerritorial:
			r.isTerritorial = v.Boolean()
		case c.primary:
			r.primary = v.String()
		case c.xmin:
			r.bbox.XMin = valueFloat(v)
			r.bboxSet++
		case c.ymin:
			r.bbox.YMin = valueFloat(v)
			r.bboxSet++
		case c.xmax:
			r.bbox.XMax = valueFloat(v)
			r.bboxSet++
		case c.ymax:
			r.bbox.YMax = valueFloat(v)
			r.bboxSet++
		}
	}
	// name: names.common["en"], else names.primary, else id; empty counts as missing.
	for i, k := range r.keys {
		if k == "en" && i < len(r.values) && r.values[i] != "" {
			r.name = r.values[i]
			break
		}
	}
	if r.name == "" {
		r.name = r.primary
	}
	if r.name == "" {
		r.name = r.id
	}
}

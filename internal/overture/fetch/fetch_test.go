package fetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/alexzeitgeist/immich-placenames/internal/cache"
	"github.com/alexzeitgeist/immich-placenames/internal/geo"
)

func TestRangeBodyTimeout(t *testing.T) {
	headers := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2")
		w.Header().Set("Content-Range", "bytes 0-1/2")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte{1})
		w.(http.Flusher).Flush()
		close(headers)
		<-r.Context().Done()
	}))
	defer server.Close()
	c := &Client{}
	client := c.client()
	if client.Timeout <= 0 || client.Timeout > 5*time.Minute {
		t.Fatalf("unbounded or excessive request timeout: %s", client.Timeout)
	}
	// Exercise the production client's body deadline without waiting minutes.
	client.Timeout = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rr := &rangeReader{c: c, ctx: ctx, url: server.URL}
	n, err := rr.once(make([]byte, 2), 0)
	select {
	case <-headers:
	default:
		t.Fatal("request did not reach response body")
	}
	if !errors.Is(err, context.DeadlineExceeded) || n != 1 {
		t.Fatalf("read: n=%d err=%v", n, err)
	}
	if ctx.Err() != nil {
		t.Fatal("parent deadline ended the request, not the HTTP timeout")
	}
}

func TestBuildConcurrent(t *testing.T) {
	c := &Client{Workers: 4, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	const groups = 80
	const rowsPerGroup = 1000
	path := filepath.Join(t.TempDir(), "out.geo")
	count, err := c.build(context.Background(), make([]groupRef, groups), cache.Header{Kind: "divisions"}, path, func(_ groupRef, add func(cache.Row, []byte) error) error {
		for i := 0; i < rowsPerGroup; i++ {
			if err := add(cache.Row{ID: "row"}, []byte{1}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := cache.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if count != groups*rowsPerGroup || len(f.Rows) != count {
		t.Fatalf("count=%d stored=%d want=%d", count, len(f.Rows), groups*rowsPerGroup)
	}
}

func TestIsAirport(t *testing.T) {
	for _, tc := range []struct {
		name, subtype, class string
		want                 bool
	}{
		{"airport", "airport", "airport", true},
		{"qualified class", "airport", "regional_airport", true},
		{"other subtype", "terminal", "airport", false},
		{"other class", "airport", "terminal", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &record{subtype: tc.subtype, class: tc.class}
			if got := r.isAirport(); got != tc.want {
				t.Errorf("isAirport() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestComplete(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  record
		want bool
	}{
		{"complete", record{bboxSet: 4, geometry: []byte{1}}, true},
		{"missing bbox", record{bboxSet: 3, geometry: []byte{1}}, false},
		{"missing geometry", record{bboxSet: 4}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.row.complete(); got != tc.want {
				t.Errorf("complete() = %t, want %t", got, tc.want)
			}
		})
	}
}

type bbox struct {
	XMin float64 `parquet:"xmin"`
	YMin float64 `parquet:"ymin"`
	XMax float64 `parquet:"xmax"`
	YMax float64 `parquet:"ymax"`
}

type names struct {
	Primary string            `parquet:"primary"`
	Common  map[string]string `parquet:"common"`
}

// serveBucket returns a client for a local bucket serving one object.
func serveBucket(t *testing.T, key string, data []byte) *http.Client {
	t.Helper()
	prefix := key[:strings.LastIndex(key, "/")+1]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("list-type") == "2" {
			if r.URL.Query().Get("prefix") != prefix {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, "<ListBucketResult><Contents><Key>%s</Key><Size>%d</Size></Contents></ListBucketResult>", key, len(data))
			return
		}
		if r.URL.Path != "/"+key {
			http.NotFound(w, r)
			return
		}
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start || end >= int64(len(data)) {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		request := r.Clone(r.Context())
		request.URL.Scheme, request.URL.Host = target.Scheme, target.Host
		return http.DefaultTransport.RoundTrip(request)
	})}
}

func TestAirportsWritesGlobalFilteredRows(t *testing.T) {
	type row struct {
		ID       string `parquet:"id"`
		Geometry []byte `parquet:"geometry"`
		Subtype  string `parquet:"subtype"`
		Class    string `parquet:"class"`
		Bbox     bbox   `parquet:"bbox"`
		Names    names  `parquet:"names"`
	}

	var parquetData bytes.Buffer
	w := parquet.NewWriter(&parquetData, parquet.SchemaOf(row{}))
	for _, r := range []row{
		{ID: "airport", Geometry: []byte{1}, Subtype: "airport", Class: "airport", Bbox: bbox{XMax: 1, YMax: 1}, Names: names{Primary: "Airport", Common: map[string]string{"en": "Airport"}}},
		{ID: "regional", Geometry: []byte{2}, Subtype: "airport", Class: "regional_airport", Bbox: bbox{XMax: 2, YMax: 2}, Names: names{Primary: "Regional", Common: map[string]string{"en": "Regional"}}},
		{ID: "terminal", Geometry: []byte{3}, Subtype: "terminal", Class: "airport", Bbox: bbox{XMax: 3, YMax: 3}, Names: names{Primary: "Terminal", Common: map[string]string{"en": "Terminal"}}},
		{ID: "station", Geometry: []byte{4}, Subtype: "airport", Class: "station", Bbox: bbox{XMax: 4, YMax: 4}, Names: names{Primary: "Station", Common: map[string]string{"en": "Station"}}},
	} {
		if err := w.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	const key = "release/test/" + infraPrefix + "airports.parquet"
	c := &Client{HTTP: serveBucket(t, key, parquetData.Bytes()), Workers: 1, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	dest := filepath.Join(t.TempDir(), "airports.geo")
	count, err := c.Airports(context.Background(), "test", dest)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("wrote %d rows, want 2", count)
	}
	f, err := cache.Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if f.Header.Kind != "airports" || f.Header.Code != "" || f.Header.Release != "test" {
		t.Fatalf("header = %+v, want global airports header", f.Header)
	}
	got := map[string]cache.Row{}
	for _, r := range f.Rows {
		got[r.ID] = r
		if r.Country != "" {
			t.Errorf("%s has country %q, want no country", r.ID, r.Country)
		}
	}
	for _, id := range []string{"airport", "regional"} {
		if _, ok := got[id]; !ok {
			t.Errorf("missing accepted airport %q", id)
		}
	}
	for _, id := range []string{"terminal", "station"} {
		if _, ok := got[id]; ok {
			t.Errorf("wrote rejected row %q", id)
		}
	}
	regional := got["regional"]
	regional.Off, regional.Len = 0, 0
	want := cache.Row{ID: "regional", Name: "Regional", Primary: "Regional", Common: map[string]string{"en": "Regional"}, Subtype: "airport", Class: "regional_airport", AdminLevel: -1, Land: true, Bbox: geo.Bbox{XMax: 2, YMax: 2}}
	if !reflect.DeepEqual(regional, want) {
		t.Errorf("regional = %+v, want %+v", regional, want)
	}
}

func TestWorldWritesCountriesAndDependencies(t *testing.T) {
	type row struct {
		ID            string `parquet:"id"`
		Geometry      []byte `parquet:"geometry"`
		Country       string `parquet:"country"`
		Subtype       string `parquet:"subtype"`
		Class         string `parquet:"class"`
		AdminLevel    int32  `parquet:"admin_level"`
		IsLand        bool   `parquet:"is_land"`
		IsTerritorial bool   `parquet:"is_territorial"`
		Bbox          bbox   `parquet:"bbox"`
		Names         names  `parquet:"names"`
	}
	en := func(n string) names { return names{Primary: n, Common: map[string]string{"en": n}} }

	var parquetData bytes.Buffer
	w := parquet.NewWriter(&parquetData, parquet.SchemaOf(row{}))
	// One row group per subtype, so admission alone decides what is read.
	for _, r := range []row{
		{ID: "china", Geometry: []byte{1}, Country: "CN", Subtype: "country", Class: "land", Bbox: bbox{XMax: 1, YMax: 1}, Names: en("China")},
		{ID: "hong kong", Geometry: []byte{2}, Country: "HK", Subtype: "dependency", Class: "land", AdminLevel: 1, IsLand: true, IsTerritorial: true, Bbox: bbox{XMax: 2, YMax: 2}, Names: en("Hong Kong")},
		{ID: "district", Geometry: []byte{3}, Country: "HK", Subtype: "region", Class: "land", Bbox: bbox{XMax: 3, YMax: 3}, Names: en("Central and Western District")},
	} {
		if err := w.Write(r); err != nil {
			t.Fatal(err)
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	const key = "release/test/" + divisionsPrefix + "divisions.parquet"
	c := &Client{HTTP: serveBucket(t, key, parquetData.Bytes()), Workers: 1, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()
	refs, err := c.plan(ctx, "test", divisionsPrefix, "subtype", worldSubtypes, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0].index != 0 || refs[1].index != 1 {
		t.Fatalf("admitted %d row groups %v, want the country and dependency groups", len(refs), refs)
	}
	dest := filepath.Join(t.TempDir(), "world.geo")
	count, err := c.World(ctx, "test", dest)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("wrote %d rows, want 2", count)
	}
	f, err := cache.Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if f.Header.Kind != "world" || f.Header.Release != "test" || !f.Header.Dependencies {
		t.Fatalf("header = %+v, want a world header carrying dependencies", f.Header)
	}
	got := map[string]cache.Row{}
	for _, r := range f.Rows {
		got[r.ID] = r
	}
	if _, ok := got["china"]; !ok {
		t.Error("missing country row")
	}
	if _, ok := got["district"]; ok {
		t.Error("wrote a region row")
	}
	hk := got["hong kong"]
	hk.Off, hk.Len = 0, 0
	want := cache.Row{ID: "hong kong", Country: "HK", Name: "Hong Kong", Primary: "Hong Kong", Common: map[string]string{"en": "Hong Kong"},
		Subtype: "dependency", Class: "land", AdminLevel: 1, Territorial: true, Land: true, Bbox: geo.Bbox{XMax: 2, YMax: 2}}
	if !reflect.DeepEqual(hk, want) {
		t.Errorf("hong kong = %+v, want %+v", hk, want)
	}
}

// The parallel readers may be the first callers of client(); an injected
// client is kept.
func TestClientBuiltOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte{1, 2})
	}))
	defer server.Close()
	c := &Client{}
	rr := &rangeReader{c: c, ctx: context.Background(), url: server.URL}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if n, err := rr.ReadAt(make([]byte, 2), 0); err != nil || n != 2 {
				t.Errorf("read: n=%d err=%v", n, err)
			}
		}()
	}
	wg.Wait()
	if c.HTTP == nil || c.client() != c.HTTP {
		t.Fatal("client not built once")
	}
	own := &http.Client{}
	if (&Client{HTTP: own}).client() != own {
		t.Fatal("injected client replaced")
	}
	defer func(rt http.RoundTripper) { http.DefaultTransport = rt }(http.DefaultTransport)
	http.DefaultTransport = http.RoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, nil }))
	if c := (&Client{}).client(); c == nil || c.Timeout == 0 {
		t.Fatal("no client without the standard default transport")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Division rows lack is_land and is_territorial; country filtering still applies.
func TestPointsWritesCountryLabels(t *testing.T) {
	type row struct {
		ID       string `parquet:"id"`
		Geometry []byte `parquet:"geometry"`
		Country  string `parquet:"country"`
		Subtype  string `parquet:"subtype"`
		Bbox     bbox   `parquet:"bbox"`
		Names    names  `parquet:"names"`
	}

	var parquetData bytes.Buffer
	w := parquet.NewWriter(&parquetData, parquet.SchemaOf(row{}))
	for _, r := range []row{
		{ID: "larsboda", Geometry: []byte{1}, Country: "SE", Subtype: "macrohood", Bbox: bbox{XMin: 18.1, YMin: 59.2, XMax: 18.1, YMax: 59.2}, Names: names{Primary: "Larsboda"}},
		{ID: "elsewhere", Geometry: []byte{2}, Country: "NO", Subtype: "locality", Bbox: bbox{XMin: 10.7, YMin: 59.9, XMax: 10.7, YMax: 59.9}, Names: names{Primary: "Oslo"}},
	} {
		if err := w.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	const key = "release/test/" + pointsPrefix + "division.parquet"
	c := &Client{HTTP: serveBucket(t, key, parquetData.Bytes()), Workers: 1, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	dest := filepath.Join(t.TempDir(), "SE.geo")
	count, err := c.Points(context.Background(), "test", "SE", geo.Bbox{XMin: 10, YMin: 55, XMax: 25, YMax: 70}, dest)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("wrote %d rows, want 1", count)
	}
	f, err := cache.Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if f.Header.Kind != "points" || f.Header.Code != "SE" || f.Header.Release != "test" {
		t.Fatalf("header = %+v, want a Swedish points header", f.Header)
	}
	got := f.Rows[0]
	got.Off, got.Len = 0, 0
	want := cache.Row{ID: "larsboda", Country: "SE", Name: "Larsboda", Primary: "Larsboda", Subtype: "macrohood",
		AdminLevel: -1, Land: true, Bbox: geo.Bbox{XMin: 18.1, YMin: 59.2, XMax: 18.1, YMax: 59.2}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("larsboda = %+v, want %+v", got, want)
	}
}

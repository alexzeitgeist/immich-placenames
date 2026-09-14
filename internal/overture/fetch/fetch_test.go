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

func TestAirportsWritesGlobalFilteredRows(t *testing.T) {
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

	const prefix = "release/test/theme=base/type=infrastructure/"
	const key = prefix + "airports.parquet"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("list-type") == "2" {
			if r.URL.Query().Get("prefix") != prefix {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, "<ListBucketResult><Contents><Key>%s</Key><Size>%d</Size></Contents></ListBucketResult>", key, parquetData.Len())
			return
		}
		if r.URL.Path != "/"+key {
			http.NotFound(w, r)
			return
		}
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start || end >= int64(parquetData.Len()) {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(parquetData.Bytes()[start : end+1])
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		request := r.Clone(r.Context())
		request.URL.Scheme, request.URL.Host = target.Scheme, target.Host
		return http.DefaultTransport.RoundTrip(request)
	})}
	c := &Client{HTTP: client, Workers: 1, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
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

package overture

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/alexzeitgeist/immich-placenames/internal/cache"
	"github.com/alexzeitgeist/immich-placenames/internal/geo"
	"github.com/alexzeitgeist/immich-placenames/internal/geocode"
)

// wkbBox is a little-endian WKB rectangle covering the box.
func wkbBox(b geo.Bbox) []byte {
	out := append([]byte{1}, 0, 0, 0, 0, 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(out[1:], 3) // polygon
	binary.LittleEndian.PutUint32(out[5:], 1) // one ring
	out = binary.LittleEndian.AppendUint32(out, 5)
	for _, c := range [][2]float64{{b.XMin, b.YMin}, {b.XMax, b.YMin}, {b.XMax, b.YMax}, {b.XMin, b.YMax}, {b.XMin, b.YMin}} {
		out = binary.LittleEndian.AppendUint64(out, math.Float64bits(c[0]))
		out = binary.LittleEndian.AppendUint64(out, math.Float64bits(c[1]))
	}
	return out
}

// area is a square centred on the origin.
type area struct {
	id, name, subtype string
	half              float64 // degrees from centre to edge
}

func writeAreas(t *testing.T, path string, areas ...area) {
	t.Helper()
	w, err := cache.NewWriter(path, cache.Header{Kind: "divisions", Code: "CH"})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range areas {
		b := geo.Bbox{XMin: -a.half, YMin: -a.half, XMax: a.half, YMax: a.half}
		row := cache.Row{ID: a.id, Country: "CH", Name: a.name, Subtype: a.subtype, AdminLevel: -1, Bbox: b}
		if err := w.Add(row, wkbBox(b)); err != nil {
			w.Abort()
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

type label struct {
	id, name, subtype string
	lon, lat          float64
}

// writeLabels writes point geometries with zero-area bounding boxes.
func writeLabels(t *testing.T, path string, labels ...label) {
	t.Helper()
	w, err := cache.NewWriter(path, cache.Header{Kind: "points", Code: "CH"})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range labels {
		point := make([]byte, 21)
		point[0], point[1] = 1, 1
		binary.LittleEndian.PutUint64(point[5:], math.Float64bits(l.lon))
		binary.LittleEndian.PutUint64(point[13:], math.Float64bits(l.lat))
		row := cache.Row{ID: l.id, Country: "CH", Name: l.name, Subtype: l.subtype, AdminLevel: -1,
			Bbox: geo.Bbox{XMin: l.lon, YMin: l.lat, XMax: l.lon, YMax: l.lat}}
		if err := w.Add(row, point); err != nil {
			w.Abort()
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func metres(c float64) func(*Candidate) {
	return func(x *Candidate) { x.Contains, x.Metres = false, c }
}

func TestPointRanking(t *testing.T) {
	cs := []Candidate{cand("macrohood", "Larsboda", 0, metres(395)), cand("locality", "Fållan", 0, metres(1590))}
	if got := pick(t, cs, selectPoint(cs, defaultSubtypes, 2000)); got != "Fållan" {
		t.Errorf("subtype order before distance, got %s", got)
	}
	if got := pick(t, cs, selectPoint(cs, defaultSubtypes, 500)); got != "Larsboda" {
		t.Errorf("within the distance only, got %s", got)
	}
	if selectPoint(cs, defaultSubtypes, 0) != -1 {
		t.Error("label selected at distance zero")
	}
	if selectPoint(cs, []string{"borough"}, 2000) != -1 {
		t.Error("unlisted subtype selected")
	}
	cs[1].Decision = "outside Stockholm Municipality"
	if got := pick(t, cs, selectPoint(cs, defaultSubtypes, 2000)); got != "Larsboda" {
		t.Errorf("guarded label selected, got %s", got)
	}
	tie := []Candidate{cand("locality", "b", 0, metres(100)), cand("locality", "a", 0, metres(100))}
	if got := pick(t, tie, selectPoint(tie, defaultSubtypes, 500)); got != "a" {
		t.Errorf("equal subtype and distance: lower id, got %s", got)
	}
}

func TestGuardArea(t *testing.T) {
	cs := []Candidate{cand("region", "Region", 0.5), cand("county", "Municipality", 0.05), cand("county", "Elsewhere", 0.01, outside)}
	if got := pick(t, cs, guardArea(cs, defaultStateSubtypes)); got != "Municipality" {
		t.Errorf("got %s", got)
	}
	if guardArea(cs, []string{"macroregion"}) != -1 {
		t.Error("guard found without a containing state subtype")
	}
	tie := []Candidate{cand("county", "b", 0.05), cand("county", "a", 0.05)}
	if got := pick(t, tie, guardArea(tie, defaultStateSubtypes)); got != "a" {
		t.Errorf("equal area: lower id, got %s", got)
	}
}

func TestDecidePoints(t *testing.T) {
	cs := []Candidate{
		cand("locality", "winner", 0, metres(100)),
		cand("locality", "far", 0, metres(900)),
		cand("terminal", "unlisted", 0, metres(100)),
		cand("locality", "loser", 0, metres(200)),
		cand("locality", "guarded", 0, metres(50)),
	}
	cs[4].Decision = "outside Home County"
	decidePoints(cs, 0, defaultSubtypes, 500)
	want := []string{"city, point", "beyond the distance", "subtype not selectable", "outranked", "outside Home County"}
	for i, c := range cs {
		if c.Decision != want[i] {
			t.Errorf("%s: decision %q, want %q", c.Name, c.Decision, want[i])
		}
	}
}

// Issue 16: point fallback respects the containing administrative area.
func TestPointFallbackNamesTheCity(t *testing.T) {
	dir := t.TempDir()
	writeCache(t, WorldPath(dir), cache.Row{ID: "country", Country: "CH", Name: "Switzerland", Subtype: "country", Bbox: unitBox})
	writeAreas(t, DivisionsPath(dir, "CH"),
		area{"region", "Big Region", "region", 1},
		area{"county", "Home County", "county", 0.002})
	// 0.001 degrees of longitude is 111 m at the equator.
	writeLabels(t, PointsPath(dir, "CH"),
		label{"near", "Near Label", "macrohood", 0.001, 0},
		label{"outside", "Outside Label", "locality", 0.003, 0},
		label{"far", "Far Label", "locality", 0.007, 0})
	for _, tc := range []struct {
		name, profile, city string
		labels              int
		decisions           map[string]string
	}{
		{name: "off by default", profile: `{"airports": false}`},
		{name: "nearest label", profile: `{"airports": false, "pointFallback": true}`, city: "Near Label", labels: 3,
			decisions: map[string]string{"near": "city, point", "outside": "outside Home County", "far": "beyond the distance"}},
		{name: "distance zero", profile: `{"airports": false, "pointFallback": true, "pointDistance": 0}`},
		{name: "beyond the distance", profile: `{"airports": false, "pointFallback": true, "pointDistance": 100}`, labels: 1,
			decisions: map[string]string{"near": "beyond the distance"}},
		{name: "area names the city", profile: `{"preferredSubtypes": ["county"], "airports": false, "pointFallback": true}`, city: "Home County"},
		{name: "no containing state area", profile: `{"airports": false, "pointFallback": true, "stateSubtypes": ["macroregion"]}`,
			city: "Outside Label", labels: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profiles, _ := writeProfileCatalog(t, dir, `{"countryOverrides": {"CH": `+tc.profile+`}}`)
			p := New(dir, profiles, nil, nil)
			defer p.Close()
			res, err := p.Resolve(context.Background(), geocode.Point{})
			if err != nil {
				t.Fatal(err)
			}
			e, err := p.Explain(context.Background(), geocode.Point{})
			if err != nil {
				t.Fatal(err)
			}
			// Resolve and Explain must agree despite their different search windows.
			if e.City != tc.city || e.Result != res {
				t.Errorf("resolve %+v, explain %+v; want city %q", res, e.Result, tc.city)
			}
			if len(e.Points) != tc.labels {
				t.Errorf("%d label candidates, want %d: %+v", len(e.Points), tc.labels, e.Points)
			}
			if _, ok := e.Releases["points/CH"]; ok != (tc.labels > 0) {
				t.Errorf("points release recorded=%t, want %t", ok, tc.labels > 0)
			}
			for _, c := range e.Points {
				if want, ok := tc.decisions[c.ID]; ok && c.Decision != want {
					t.Errorf("%s: decision %q, want %q", c.ID, c.Decision, want)
				}
			}
			want := tc.city
			if want == "" {
				want = "Big Region" // no label: the state fills the city
			}
			if res.City != want {
				t.Errorf("city %q, want %q", res.City, want)
			}
		})
	}
}

func TestPointFallbackSphericalBounds(t *testing.T) {
	// Place the easternmost label slightly poleward, just inside 2 km
	// to avoid rounding at the distance limit.
	a, phi := 1999.98/6371000.0, 89*math.Pi/180
	edgeLon := math.Asin(math.Sin(a)/math.Cos(phi)) * 180 / math.Pi
	edgeLat := math.Asin(math.Sin(phi)/math.Cos(a)) * 180 / math.Pi
	for _, tc := range []struct {
		name      string
		pt, label geocode.Point
		bound     float64
		wantCity  string
	}{
		{"dateline east", geocode.Point{Lat: -16, Lon: 179.999}, geocode.Point{Lat: -16, Lon: -179.999}, 500, "Nearby City"},
		{"dateline west", geocode.Point{Lat: -16, Lon: -179.999}, geocode.Point{Lat: -16, Lon: 179.999}, 500, "Nearby City"},
		{"northern oblique edge", geocode.Point{Lat: 89}, geocode.Point{Lat: edgeLat, Lon: edgeLon}, 2000, "Nearby City"},
		{"southern oblique edge", geocode.Point{Lat: -89}, geocode.Point{Lat: -edgeLat, Lon: edgeLon}, 2000, "Nearby City"},
		{"across north pole", geocode.Point{Lat: 89.999}, geocode.Point{Lat: 89.999, Lon: 180}, 500, "Nearby City"},
		{"across south pole", geocode.Point{Lat: -89.999}, geocode.Point{Lat: -89.999, Lon: -180}, 500, "Nearby City"},
		{"at north pole", geocode.Point{Lat: 90, Lon: 170}, geocode.Point{Lat: 89.999, Lon: -170}, 500, "Nearby City"},
		{"at south pole", geocode.Point{Lat: -90, Lon: -170}, geocode.Point{Lat: -89.999, Lon: 170}, 500, "Nearby City"},
		{"antipodal label", geocode.Point{Lat: 89.001}, geocode.Point{Lat: -89.001, Lon: 180}, 150000, ""},
		{"square corner outside circle", geocode.Point{}, geocode.Point{Lat: 0.004, Lon: 0.004}, 500, ""},
		{"across dateline beyond bound", geocode.Point{Lat: -16, Lon: 179.999}, geocode.Point{Lat: -16, Lon: -179.995}, 500, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			worldBox := geo.Bbox{XMin: -180, YMin: -90, XMax: 180, YMax: 90}
			writeCacheAt(t, WorldPath(dir), tc.pt, cache.Row{ID: "country", Country: "CH", Name: "Test Country", Subtype: "country", Bbox: worldBox})
			// The guard covers both sides of the dateline.
			w, err := cache.NewWriter(DivisionsPath(dir, "CH"), cache.Header{Kind: "divisions", Code: "CH"})
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Add(cache.Row{ID: "region", Country: "CH", Name: "Test Region", Subtype: "region", Bbox: worldBox}, wkbBox(worldBox)); err != nil {
				w.Abort()
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			writeLabels(t, PointsPath(dir, "CH"), label{"label", "Nearby City", "locality", tc.label.Lon, tc.label.Lat})
			profiles, _ := writeProfileCatalog(t, dir, fmt.Sprintf(`{"defaultProfile":{"airports":false,"pointFallback":true,"pointDistance":%g}}`, tc.bound))
			p := New(dir, profiles, nil, nil)
			defer p.Close()
			res, err := p.Resolve(context.Background(), tc.pt)
			if err != nil {
				t.Fatal(err)
			}
			e, err := p.Explain(context.Background(), tc.pt)
			if err != nil {
				t.Fatal(err)
			}
			if e.City != tc.wantCity || res.State != "Test Region" || res != e.Result {
				t.Errorf("label %.8f m away, bound %g: Resolve=%+v, Explain=%+v; want city %q",
					geo.Haversine(tc.pt.Lat, tc.pt.Lon, tc.label.Lat, tc.label.Lon), tc.bound, res, e.Result, tc.wantCity)
			}
			if len(e.Points) != 1 {
				t.Fatalf("explanation has %d labels, want one", len(e.Points))
			}
			if c := e.Points[0]; c.Distance != 0 || c.Metres <= 0 || math.IsNaN(c.Metres) || math.IsInf(c.Metres, 0) {
				t.Errorf("label must report metres only: distance=%g, metres=%g", c.Distance, c.Metres)
			}
			wantDecision := "city, point"
			if tc.wantCity == "" {
				wantDecision = "beyond the distance"
			}
			if e.Points[0].Decision != wantDecision {
				t.Errorf("decision %q, want %q", e.Points[0].Decision, wantDecision)
			}
		})
	}
}

type pointFetcher struct {
	t     *testing.T
	fail  bool
	calls int
}

func (f *pointFetcher) World(context.Context, string, string) error {
	return errors.New("world fetched")
}

func (f *pointFetcher) Divisions(context.Context, string, geo.Bbox, string) error {
	return errors.New("divisions fetched")
}

func (f *pointFetcher) Airports(context.Context, string) error {
	return errors.New("airports fetched")
}

func (f *pointFetcher) Points(_ context.Context, _ string, _ geo.Bbox, dest string) error {
	f.calls++
	if f.fail {
		return errors.New("offline")
	}
	writeLabels(f.t, dest, label{"near", "Fetched Label", "locality", 0.001, 0})
	return nil
}

// Failed fetches are remembered for subsequent lookups.
func TestPointsFetchedOnDemand(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile string
		fail    bool
		city    string
		calls   int
		err     bool
	}{
		{"fetched", `{"airports": false, "pointFallback": true}`, false, "Fetched Label", 1, false},
		{"off", `{"airports": false}`, false, "Big Region", 0, false},
		{"failed", `{"airports": false, "pointFallback": true}`, true, "", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeCache(t, WorldPath(dir), cache.Row{ID: "country", Country: "CH", Name: "Switzerland", Subtype: "country", Bbox: unitBox})
			writeAreas(t, DivisionsPath(dir, "CH"), area{"region", "Big Region", "region", 1})
			profiles, _ := writeProfileCatalog(t, dir, `{"countryOverrides": {"CH": `+tc.profile+`}}`)
			f := &pointFetcher{t: t, fail: tc.fail}
			p := New(dir, profiles, f, nil)
			defer p.Close()
			res, err := p.Resolve(context.Background(), geocode.Point{})
			_, again := p.Resolve(context.Background(), geocode.Point{})
			if (err != nil) != tc.err || (again != nil) != tc.err || res.City != tc.city || f.calls != tc.calls {
				t.Errorf("got %q, %v, again %v, %d fetches; want %q, %d fetches", res.City, err, again, f.calls, tc.city, tc.calls)
			}
		})
	}
}

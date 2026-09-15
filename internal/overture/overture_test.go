package overture

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/alexzeitgeist/immich-placenames/internal/cache"
	"github.com/alexzeitgeist/immich-placenames/internal/geo"
	"github.com/alexzeitgeist/immich-placenames/internal/geocode"
)

func cand(subtype, name string, area float64, opts ...func(*Candidate)) Candidate {
	c := Candidate{ID: name, Name: name, Subtype: subtype, Class: "land", Country: "CH", AdminLevel: -1,
		Bbox: geo.Bbox{XMin: 0, YMin: 0, XMax: area, YMax: 1}, Contains: true}
	for _, o := range opts {
		o(&c)
	}
	return c
}

func outside(c *Candidate)            { c.Contains = false }
func territorial(c *Candidate)        { c.Territorial = true }
func level(l int32) func(*Candidate)  { return func(c *Candidate) { c.AdminLevel = l } }
func class(s string) func(*Candidate) { return func(c *Candidate) { c.Class = s } }

func pick(t *testing.T, cands []Candidate, i int) string {
	t.Helper()
	if i < 0 {
		return ""
	}
	return cands[i].Name
}

// Rule tests ported from Immich ReverseGeo; see NOTICE for attribution.

func TestCountryPrefersTerritorialThenSmallerArea(t *testing.T) {
	cs := []Candidate{cand("country", "land", 0.5), cand("country", "territorial", 0.9, territorial)}
	if got := pick(t, cs, rankCountry(cs)); got != "territorial" {
		t.Errorf("territorial before area, got %s", got)
	}
	cs = []Candidate{cand("country", "broad", 0.9), cand("country", "tight", 0.5)}
	if got := pick(t, cs, rankCountry(cs)); got != "tight" {
		t.Errorf("smaller area, got %s", got)
	}
	cs = []Candidate{cand("country", "bbox only", 0.01, outside), cand("country", "contains", 0.9)}
	if got := pick(t, cs, rankCountry(cs)); got != "contains" {
		t.Errorf("geometry containment, got %s", got)
	}
	if rankCountry([]Candidate{cand("country", "x", 1, outside)}) != -1 {
		t.Error("bbox-only candidate selected")
	}
}

func TestStatePrefersRegionOverCounty(t *testing.T) {
	cs := []Candidate{cand("county", "Zurich District", 0.05), cand("region", "Canton of Zurich", 0.20)}
	if got := pick(t, cs, selectName(cs, defaultStateSubtypes, false)); got != "Canton of Zurich" {
		t.Errorf("got %s", got)
	}
}

func TestStatePrefersLowerAdminLevelWithinSameSubtype(t *testing.T) {
	cs := []Candidate{cand("region", "Lower Priority Region", 0.01, level(2)), cand("region", "Preferred Region", 0.20, level(1))}
	if got := pick(t, cs, selectName(cs, defaultStateSubtypes, false)); got != "Preferred Region" {
		t.Errorf("got %s", got)
	}
}

func TestMissingAdminLevelSortsLast(t *testing.T) {
	cs := []Candidate{cand("region", "Unknown Level", 0.01), cand("region", "Level 2", 0.20, level(2))}
	if got := pick(t, cs, selectName(cs, defaultStateSubtypes, false)); got != "Level 2" {
		t.Errorf("got %s", got)
	}
}

func TestCityPrefersLocalityOverNeighborhood(t *testing.T) {
	cs := []Candidate{cand("neighborhood", "Seefeld", 0.01), cand("locality", "Zurich", 0.20)}
	p := (&Profiles{}).Profile("CH")
	if got := pick(t, cs, selectName(cs, p.PreferredSubtypes, false)); got != "Zurich" {
		t.Errorf("got %s", got)
	}
}

func TestCityRequiresGeometryContainment(t *testing.T) {
	cs := []Candidate{cand("locality", "Zurich", 0.20, outside), cand("locality", "Zurich-Flughafen", 0.05)}
	if got := pick(t, cs, selectName(cs, defaultSubtypes, false)); got != "Zurich-Flughafen" {
		t.Errorf("got %s", got)
	}
	cs = []Candidate{cand("locality", "Zurich", 0.20, outside)}
	if selectName(cs, defaultSubtypes, false) != -1 {
		t.Error("bbox-only locality selected without geometry containment")
	}
}

func TestCityPrefersTerritorialWithinSameSubtype(t *testing.T) {
	cs := []Candidate{cand("locality", "Water Label", 0.01), cand("locality", "Real Administrative Area", 0.02, territorial)}
	if got := pick(t, cs, selectName(cs, defaultSubtypes, false)); got != "Real Administrative Area" {
		t.Errorf("got %s", got)
	}
}

func TestCityUsesConfiguredSubtypeOrder(t *testing.T) {
	cs := []Candidate{cand("locality", "Chassieu", 0.00217), cand("localadmin", "Lyon", 0.39414)}
	if got := pick(t, cs, selectName(cs, []string{"localadmin", "locality"}, true)); got != "Lyon" {
		t.Errorf("got %s", got)
	}
}

func TestCityUsesLargestAreaTieBreak(t *testing.T) {
	cs := []Candidate{cand("locality", "Armfelt", 0.000239), cand("locality", "Salo", 0.60364)}
	if got := pick(t, cs, selectName(cs, []string{"locality", "localadmin"}, true)); got != "Salo" {
		t.Errorf("got %s", got)
	}
	if got := pick(t, cs, selectName(cs, []string{"locality", "localadmin"}, false)); got != "Armfelt" {
		t.Errorf("smallest: got %s", got)
	}
}

func TestCityUsesConfiguredCountyPreference(t *testing.T) {
	cs := []Candidate{cand("locality", "Altstadt-Lehel", 0.000856), cand("county", "Munich", 0.067538, level(2))}
	if got := pick(t, cs, selectName(cs, []string{"county", "locality", "localadmin"}, true)); got != "Munich" {
		t.Errorf("got %s", got)
	}
}

func TestSubtypeSpecificityBeforeArea(t *testing.T) {
	cs := []Candidate{cand("neighborhood", "hood", 0.20), cand("locality", "town", 0.05)}
	if got := pick(t, cs, selectName(cs, []string{"neighborhood", "locality"}, false)); got != "hood" {
		t.Errorf("got %s", got)
	}
}

func TestAirportRanking(t *testing.T) {
	pt := geocode.Point{Lat: 0.5, Lon: 0.5}
	a := cand("airport", "heli", 1, class("heliport"))
	b := cand("airport", "intl", 1, class("international_airport"))
	cs := []Candidate{a, b}
	if got := pick(t, cs, selectAirport(cs, pt)); got != "intl" {
		t.Errorf("class rank, got %s", got)
	}
	near := cand("airport", "near", 1, class("airport"))
	far := cand("airport", "far", 1, class("airport"))
	far.Bbox = geo.Bbox{XMin: 0, YMin: 0, XMax: 1, YMax: 3}
	cs = []Candidate{far, near}
	if got := pick(t, cs, selectAirport(cs, pt)); got != "near" {
		t.Errorf("equal class: nearer bbox centre, got %s", got)
	}
	tie1, tie2 := cand("airport", "b", 1, class("airport")), cand("airport", "a", 1, class("airport"))
	cs = []Candidate{tie1, tie2}
	if got := pick(t, cs, selectAirport(cs, pt)); got != "a" {
		t.Errorf("equal class and distance: lower id, got %s", got)
	}
	cs = []Candidate{cand("airport", "out", 1, class("international_airport"), outside)}
	if selectAirport(cs, pt) != -1 {
		t.Error("non-containing airport selected")
	}
}

func f64(v float64) *float64 { return &v }
func boolp(v bool) *bool     { return &v }

func TestProfileMerge(t *testing.T) {
	dir := t.TempDir()
	user := filepath.Join(dir, "profiles.json")
	os.WriteFile(user, []byte(`{
	  "defaultProfile": {"preferredSubtypes": [], "tieBreakMode": "", "fallbackDistance": 0.005, "language": "primary"},
	  "countryOverrides": {
	    "hr": {"preferredSubtypes": ["County", "locality", " county "], "tieBreakMode": "largest-area", "airports": false, "stateSubtypes": ["County", " region "]},
	    "IN": {"preferredSubtypes": ["locality", "county"], "tieBreakMode": "", "fallbackDistance": 0, "language": " EN "},
	    "DE": {"preferredSubtypes": [], "tieBreakMode": "smallest-area"},
	    "FR": {"language": ["DE", "en", " "]},
	    "XX": {"preferredSubtypes": ["  "], "tieBreakMode": " Largest-Area ", "stateSubtypes": ["  "]}
	  }}`), 0o644)
	p, err := LoadProfiles(user)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]Profile{
		"CH": {defaultSubtypes, TieBreakSmallest, f64(0.005), boolp(true), Languages{"primary", "en"}, defaultStateSubtypes, boolp(false), f64(500), nil},
		"HR": {[]string{"county", "locality"}, TieBreakLargest, f64(0.005), boolp(false), Languages{"primary", "en"}, []string{"county", "region"}, boolp(false), f64(500), nil},
		"IN": {[]string{"locality", "county"}, TieBreakSmallest, f64(0), boolp(true), Languages{"en", "primary"}, defaultStateSubtypes, boolp(false), f64(500), nil},
		"DE": {[]string{"county", "locality", "localadmin", "borough", "macrohood", "neighborhood", "microhood"}, TieBreakSmallest, f64(0.005), boolp(true), Languages{"primary", "en"}, defaultStateSubtypes, boolp(false), f64(500), nil},
		"FR": {[]string{"localadmin", "locality", "county", "borough", "macrohood", "neighborhood", "microhood"}, TieBreakLargest, f64(0.005), boolp(true), Languages{"de", "en", "primary"}, defaultStateSubtypes, boolp(false), f64(500), nil},
		"XX": {defaultSubtypes, TieBreakLargest, f64(0.005), boolp(true), Languages{"primary", "en"}, defaultStateSubtypes, boolp(false), f64(500), nil},
	}
	for code, want := range cases {
		if got := p.Profile(code); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v want %v", code, got, want)
		}
	}
	bundled, _ := LoadProfiles("")
	if got := bundled.Profile("de"); got.TieBreakMode != TieBreakLargest || *got.FallbackDistance != DefaultFallbackDistance || !*got.Airports || !reflect.DeepEqual(got.Language, Languages{"en", "primary"}) {
		t.Errorf("bundled DE: %v", got)
	}
	for name, body := range map[string]string{
		"unknown key":  `{"defaultProfile": {"fallbackDistanc": 1}}`,
		"tie-break":    `{"countryOverrides": {"HR": {"tieBreakMode": "largest"}}}`,
		"negative":     `{"countryOverrides": {"HR": {"fallbackDistance": -1}}}`,
		"subtype":      `{"defaultProfile": {"preferredSubtypes": ["localiity"]}}`,
		"state":        `{"countryOverrides": {"HR": {"stateSubtypes": ["regoin"]}}}`,
		"key":          `{"countryOverrides": {"HRV": {"airports": false}}}`,
		"language":     `{"defaultProfile": {"language": "d e"}}`,
		"languages":    `{"defaultProfile": {"language": ["de", 1]}}`,
		"trailing":     `{"defaultProfile": {}} }`,
		"two catalogs": `{"defaultProfile": {}} {"countryOverrides": {"HR": {"airports": false}}}`,
	} {
		os.WriteFile(user, []byte(body), 0o644)
		if _, err := LoadProfiles(user); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// writeCache writes rows that all carry a WKB point geometry at (0, 0).
func writeCache(t *testing.T, path string, rows ...cache.Row) {
	t.Helper()
	writeCacheAt(t, path, geocode.Point{}, rows...)
}

// writeCacheAt writes rows that all carry a WKB point geometry at the point.
// Dependencies prevents automatic refetching in tests.
func writeCacheAt(t *testing.T, path string, at geocode.Point, rows ...cache.Row) {
	t.Helper()
	writeCacheHeader(t, path, cache.Header{Kind: "test", Code: "CH", Dependencies: true}, at, rows...)
}

func writeCacheHeader(t *testing.T, path string, hdr cache.Header, at geocode.Point, rows ...cache.Row) {
	t.Helper()
	point := make([]byte, 21) // little-endian WKB point
	point[0], point[1] = 1, 1
	binary.LittleEndian.PutUint64(point[5:], math.Float64bits(at.Lon))
	binary.LittleEndian.PutUint64(point[13:], math.Float64bits(at.Lat))
	w, err := cache.NewWriter(path, hdr)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if err := w.Add(r, point); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

var unitBox = geo.Bbox{XMin: -1, YMin: -1, XMax: 1, YMax: 1}

// A profile with airports false leaves the city to the divisions.
func TestAirportsOffByProfile(t *testing.T) {
	dir := t.TempDir()
	writeCache(t, WorldPath(dir), cache.Row{ID: "c", Country: "CH", Name: "Switzerland", Subtype: "country", Bbox: unitBox})
	writeCache(t, DivisionsPath(dir, "CH"), cache.Row{ID: "l", Country: "CH", Name: "Real City", Subtype: "locality", Bbox: unitBox})
	writeCache(t, AirportsPath(dir), cache.Row{ID: "a", Name: "Real Airport", Subtype: "airport", Class: "airport", Bbox: unitBox})
	user := filepath.Join(dir, "profiles.json")
	os.WriteFile(user, []byte(`{"countryOverrides": {"CH": {"airports": false}}}`), 0o644)
	for _, c := range []struct{ path, city string }{{"", "Real Airport"}, {user, "Real City"}} {
		profiles, err := LoadProfiles(c.path)
		if err != nil {
			t.Fatal(err)
		}
		p := New(dir, profiles, nil, nil)
		res, err := p.Resolve(context.Background(), geocode.Point{})
		p.Close()
		if err != nil || res.City != c.city {
			t.Errorf("profiles %q: got %q, %v; want %q", c.path, res.City, err, c.city)
		}
	}
}

// A profile's stateSubtypes picks the state as preferredSubtypes picks the
// city, containing or nearest within the fallback distance.
func TestStateByProfile(t *testing.T) {
	dir := t.TempDir()
	writeCache(t, WorldPath(dir), cache.Row{ID: "c", Country: "CH", Name: "Switzerland", Subtype: "country", Bbox: unitBox})
	writeCache(t, AirportsPath(dir), cache.Row{ID: "a", Name: "Far Airport", Subtype: "airport", Class: "airport", Bbox: geo.Bbox{XMin: 10, YMin: 10, XMax: 11, YMax: 11}})
	user := filepath.Join(dir, "profiles.json")
	os.WriteFile(user, []byte(`{"countryOverrides": {"CH": {"stateSubtypes": ["county"]}}}`), 0o644)
	for _, at := range []geocode.Point{{}, {Lon: 0.005}} {
		writeCacheAt(t, DivisionsPath(dir, "CH"), at,
			cache.Row{ID: "r", Country: "CH", Name: "Real Region", Subtype: "region", Bbox: unitBox},
			cache.Row{ID: "d", Country: "CH", Name: "Real District", Subtype: "county", Bbox: unitBox})
		for _, c := range []struct{ path, state string }{{"", "Real Region"}, {user, "Real District"}} {
			profiles, err := LoadProfiles(c.path)
			if err != nil {
				t.Fatal(err)
			}
			p := New(dir, profiles, nil, nil)
			res, err := p.Resolve(context.Background(), geocode.Point{})
			p.Close()
			if err != nil || res.State != c.state {
				t.Errorf("divisions at %v, profiles %q: got %q, %v; want %q", at, c.path, res.State, err, c.state)
			}
		}
	}
}

// airportFetcher fetches airports only: one containing airport, or a failure.
type airportFetcher struct {
	t     *testing.T
	fail  bool
	calls int
}

func (f *airportFetcher) World(context.Context, string, string) error {
	return errors.New("world fetched")
}
func (f *airportFetcher) Divisions(context.Context, string, geo.Bbox, string) error {
	return errors.New("divisions fetched")
}
func (f *airportFetcher) Points(context.Context, string, geo.Bbox, string) error {
	return errors.New("division points fetched")
}
func (f *airportFetcher) Airports(_ context.Context, dest string) error {
	f.calls++
	if f.fail {
		return errors.New("offline")
	}
	writeCache(f.t, dest, cache.Row{ID: "a", Name: "Real Airport", Subtype: "airport", Class: "airport", Bbox: unitBox})
	return nil
}

// Airport fetching follows the profile. A fetch failure is cached and fails
// subsequent lookups that require airports.
func TestAirportsFetchedOnDemand(t *testing.T) {
	dir := t.TempDir()
	writeCache(t, WorldPath(dir), cache.Row{ID: "c", Country: "CH", Name: "Switzerland", Subtype: "country", Bbox: unitBox})
	writeCache(t, DivisionsPath(dir, "CH"), cache.Row{ID: "l", Country: "CH", Name: "Real City", Subtype: "locality", Bbox: unitBox})
	user := filepath.Join(dir, "profiles.json")
	os.WriteFile(user, []byte(`{"countryOverrides": {"CH": {"airports": false}}}`), 0o644)
	for _, c := range []struct {
		name  string
		path  string
		fail  bool
		city  string
		calls int
		err   bool
	}{
		{"fetched", "", false, "Real Airport", 1, false},
		{"off", user, false, "Real City", 0, false},
		{"failed", "", true, "", 1, true},
	} {
		profiles, err := LoadProfiles(c.path)
		if err != nil {
			t.Fatal(err)
		}
		f := &airportFetcher{t: t, fail: c.fail}
		p := New(dir, profiles, f, nil)
		res, err := p.Resolve(context.Background(), geocode.Point{})
		_, again := p.Resolve(context.Background(), geocode.Point{})
		p.Close()
		if (err != nil) != c.err || (again != nil) != c.err || res.City != c.city || f.calls != c.calls {
			t.Errorf("%s: got %q, %v, again %v, %d fetches; want %q, %d fetches", c.name, res.City, err, again, f.calls, c.city, c.calls)
		}
		os.Remove(AirportsPath(dir))
	}
}

func dist(d float64) func(*Candidate) {
	return func(c *Candidate) { c.Contains, c.Distance = false, d }
}

func TestNearestFallbackWithinBound(t *testing.T) {
	cs := []Candidate{cand("locality", "Cavtat", 0.01, dist(0.00026)), cand("county", "Konavle", 0.5, dist(0.00026)), cand("locality", "Far", 0.01, dist(0.02))}
	if got := pick(t, cs, selectNearest(cs, []string{"county", "locality"}, DefaultFallbackDistance)); got != "Konavle" {
		t.Errorf("subtype order before distance, got %s", got)
	}
	if got := pick(t, cs, selectNearest(cs, defaultSubtypes, DefaultFallbackDistance)); got != "Cavtat" {
		t.Errorf("nearest listed subtype, got %s", got)
	}
	if selectNearest(cs, defaultSubtypes, 0) != -1 {
		t.Error("fallback selected at bound zero")
	}
	cs = []Candidate{cand("locality", "Far", 0.01, dist(0.02)), cand("locality", "Inside", 0.01)}
	if selectNearest(cs, defaultSubtypes, DefaultFallbackDistance) != -1 {
		t.Error("beyond the bound or containing candidate selected")
	}
}

type worldFetcher struct {
	t          *testing.T
	fail       bool
	worlds     int
	release    string
	divisions  int
	noDivision bool
}

func (f *worldFetcher) World(_ context.Context, release, dest string) error {
	f.worlds++
	f.release = release
	if f.fail {
		return errors.New("offline")
	}
	writeCache(f.t, dest,
		cache.Row{ID: "cn", Country: "CN", Name: "China", Subtype: "country", Bbox: unitBox},
		cache.Row{ID: "hk", Country: "HK", Name: "Hong Kong", Subtype: "dependency", Bbox: geo.Bbox{XMin: -0.5, YMin: -0.5, XMax: 0.5, YMax: 0.5}})
	return nil
}

func (f *worldFetcher) Divisions(_ context.Context, code string, _ geo.Bbox, dest string) error {
	f.divisions++
	if f.noDivision {
		return fmt.Errorf("%s: %w", dest, cache.ErrNoRows)
	}
	writeCache(f.t, dest, cache.Row{ID: "l", Country: code, Name: "Real City", Subtype: "locality", Bbox: unitBox})
	return nil
}

func (f *worldFetcher) Points(context.Context, string, geo.Bbox, string) error {
	return errors.New("division points fetched")
}

func (f *worldFetcher) Airports(context.Context, string) error { return errors.New("airports fetched") }

// Migration runs once per provider; failed or disabled fetches keep the old cache.
func TestWorldRefetchedForDependencies(t *testing.T) {
	profiles, err := LoadProfiles("")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		fetch   bool
		fail    bool
		country string
		worlds  int
	}{
		{"refetched", true, false, "Hong Kong", 1},
		{"refetch failed", true, true, "China", 1},
		{"no fetcher", false, false, "China", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeCacheHeader(t, WorldPath(dir), cache.Header{Kind: "world", Release: "2026-08-19.0"}, geocode.Point{},
				cache.Row{ID: "cn", Country: "CN", Name: "China", Subtype: "country", Bbox: unitBox})
			writeCache(t, DivisionsPath(dir, "CN"), cache.Row{ID: "l", Country: "CN", Name: "Real City", Subtype: "locality", Bbox: unitBox})
			writeCache(t, DivisionsPath(dir, "HK"), cache.Row{ID: "l", Country: "HK", Name: "Real City", Subtype: "locality", Bbox: unitBox})
			f := &worldFetcher{t: t, fail: c.fail}
			var fetcher Fetcher
			if c.fetch {
				fetcher = f
			}
			p := New(dir, profiles, fetcher, nil)
			p.Overrides.Airports = boolp(false)
			defer p.Close()
			res, err := p.Resolve(context.Background(), geocode.Point{})
			again, errAgain := p.Resolve(context.Background(), geocode.Point{})
			if err != nil || errAgain != nil {
				t.Fatalf("resolve: %v, %v", err, errAgain)
			}
			if c.fetch && f.release != "2026-08-19.0" {
				t.Errorf("migration release %q, want 2026-08-19.0", f.release)
			}
			if res.Country != c.country || again.Country != c.country || f.worlds != c.worlds {
				t.Errorf("country %q then %q after %d fetches; want %q after %d", res.Country, again.Country, f.worlds, c.country, c.worlds)
			}
		})
	}
}

// An empty divisions fetch fails the asset and is not retried in this pass.
func TestEmptyDivisionsFailsResolution(t *testing.T) {
	dir := t.TempDir()
	writeCache(t, WorldPath(dir), cache.Row{ID: "bv", Country: "BV", Name: "Bouvet Island", Subtype: "dependency", Bbox: unitBox})
	profiles, err := LoadProfiles("")
	if err != nil {
		t.Fatal(err)
	}
	f := &worldFetcher{t: t, noDivision: true}
	p := New(dir, profiles, f, nil)
	p.Overrides.Airports = boolp(false)
	defer p.Close()
	for range 2 {
		res, err := p.Resolve(context.Background(), geocode.Point{})
		if !errors.Is(err, cache.ErrNoRows) || res != (geocode.Result{}) {
			t.Fatalf("got %+v, %v; want ErrNoRows and no result", res, err)
		}
	}
	if f.divisions != 1 {
		t.Fatalf("fetched divisions %d times, want 1", f.divisions)
	}
}

// Bounded lookup must preserve the result; Explain must keep exact distances.
func TestResolveMatchesExplain(t *testing.T) {
	fallback := 0.01
	for _, c := range []struct {
		at   geocode.Point
		city string
	}{
		{geocode.Point{}, "Test City"},
		{geocode.Point{Lon: 0.005}, "Test City"},
		{geocode.Point{Lon: 0.5}, ""},
	} {
		t.Run(fmt.Sprintf("city at %g", c.at.Lon), func(t *testing.T) {
			dir := t.TempDir()
			writeCache(t, WorldPath(dir), cache.Row{ID: "c", Country: "CH", Name: "Switzerland", Subtype: "country", Bbox: unitBox})
			// Keep the city in the bbox candidates while moving its geometry.
			writeCacheAt(t, DivisionsPath(dir, "CH"), c.at,
				cache.Row{ID: "l", Country: "CH", Name: "Test City", Subtype: "locality", Bbox: unitBox})
			profiles, err := LoadProfiles("")
			if err != nil {
				t.Fatal(err)
			}
			p := New(dir, profiles, nil, nil)
			defer p.Close()
			p.Overrides.Airports, p.Overrides.FallbackDistance = boolp(false), &fallback
			res, err := p.Resolve(context.Background(), geocode.Point{})
			if err != nil {
				t.Fatal(err)
			}
			e, err := p.Explain(context.Background(), geocode.Point{})
			if err != nil {
				t.Fatal(err)
			}
			if res.City != c.city || e.Result != res {
				t.Errorf("resolve %+v, explain %+v; want city %q", res, e.Result, c.city)
			}
			if d := e.Divisions[0].Distance; math.Abs(d-c.at.Lon) > 1e-9 {
				t.Errorf("explain distance %g, want %g", d, c.at.Lon)
			}
		})
	}
}

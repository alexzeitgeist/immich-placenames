package overture

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/alexzeitgeist/immich-placenames/internal/cache"
	"github.com/alexzeitgeist/immich-placenames/internal/geo"
	"github.com/alexzeitgeist/immich-placenames/internal/geocode"
)

func TestCityFallbackFillsAnEmptyCity(t *testing.T) {
	dir := t.TempDir()
	writeCache(t, WorldPath(dir), cache.Row{ID: "country", Country: "US", Name: "United States", Subtype: "country", Bbox: unitBox})
	writeCache(t, DivisionsPath(dir, "US"), cache.Row{ID: "region", Country: "US", Name: "Florida", Subtype: "region", Bbox: unitBox})
	// Keep airports enabled without matching one.
	writeCacheAt(t, AirportsPath(dir), geocode.Point{Lat: 0.5, Lon: 0.5},
		cache.Row{ID: "airport", Name: "Everglades Air Park", Subtype: "airport", Class: "airfield",
			Bbox: geo.Bbox{XMin: 0.4, YMin: 0.4, XMax: 0.6, YMax: 0.6}})
	for _, tc := range []struct{ name, profile, city, from string }{
		{"state by default", `{}`, "Florida", geocode.SourceState},
		{"state before country", `{"cityFallback": ["state", "country"]}`, "Florida", geocode.SourceState},
		{"country alone", `{"cityFallback": ["country"]}`, "United States", geocode.SourceCountry},
		{"disabled", `{"cityFallback": []}`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profiles, _ := writeProfileCatalog(t, t.TempDir(), `{"countryOverrides": {"US": `+tc.profile+`}}`)
			p := New(dir, profiles, nil, nil)
			defer p.Close()
			e, err := p.Explain(context.Background(), geocode.Point{})
			if err != nil {
				t.Fatal(err)
			}
			if e.City != "" || e.Airport != "" {
				t.Fatalf("fixture named a city: %+v", e)
			}
			if e.Result.City != tc.city || e.CityFilledFrom != tc.from {
				t.Errorf("city %q from %q; want %q from %q", e.Result.City, e.CityFilledFrom, tc.city, tc.from)
			}
			if e.Result.State != "Florida" || e.Result.Country != "United States" || !e.Result.Writable() {
				t.Errorf("the fallback changed more than the city: %+v", e.Result)
			}
			want := map[string]int{}
			if tc.from != "" {
				want[tc.from] = 1
			}
			if !reflect.DeepEqual(p.Filled(), want) {
				t.Errorf("counted %v, want %v", p.Filled(), want)
			}
		})
	}
}

// An empty list clears the inherited sources; a missing one inherits them.
func TestCityFallbackMergeAndText(t *testing.T) {
	both := []string{geocode.SourceState, geocode.SourceCountry}
	countryFirst := []string{geocode.SourceCountry, geocode.SourceState}
	profiles, path := writeProfileCatalog(t, t.TempDir(), `{
  "defaultProfile": {"cityFallback": ["Country ", "country", "state"]},
  "countryOverrides": {"SE": {"cityFallback": []}, "US": {}}
}`)
	for _, tc := range []struct {
		name string
		got  []string
		want []string
	}{
		{"user default", *profiles.Default().CityFallback, countryFirst},
		{"inherited", *profiles.Profile("US").CityFallback, countryFirst},
		{"cleared", *profiles.Profile("SE").CityFallback, []string{}},
	} {
		if !reflect.DeepEqual(tc.got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, tc.got, tc.want)
		}
	}
	bundled, err := LoadProfiles("")
	if err != nil {
		t.Fatal(err)
	}
	if got := *bundled.Default().CityFallback; !reflect.DeepEqual(got, both) {
		t.Errorf("bundled default: got %v, want %v", got, both)
	}
	if got, want := *bundled.Profile("US").CityFallback, []string{"county", geocode.SourceState, geocode.SourceCountry}; !reflect.DeepEqual(got, want) {
		t.Errorf("bundled US: got %v, want %v", got, want)
	}
	if got := bundled.Default().String(); strings.Contains(got, "cityFallback") {
		t.Errorf("the default chain needs no mention: %q", got)
	}
	if got := profiles.Default().String(); !strings.Contains(got, "cityFallback=[country state]") {
		t.Errorf("profile text: %q", got)
	}
	if got := profiles.Profile("SE").String(); !strings.Contains(got, "cityFallback=[]") {
		t.Errorf("SE profile text: %q", got)
	}
	for name, body := range map[string]string{
		"unknown source": `{"defaultProfile": {"cityFallback": ["city"]}}`,
		"blank source":   `{"defaultProfile": {"cityFallback": [" "]}}`,
		"not a list":     `{"defaultProfile": {"cityFallback": "state"}}`,
		"typo":           `{"defaultProfile": {"cityFallbck": ["state"]}}`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadProfiles(path); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// Test squares are centred at (cx, 0). Cache bounding boxes all cover the
// origin, so geometry determines containment after the bbox check.
type division struct {
	id, name, subtype string
	cx, half          float64
}

func writeDivisions(t *testing.T, path string, list ...division) {
	t.Helper()
	w, err := cache.NewWriter(path, cache.Header{Kind: "divisions", Code: "CH", Dependencies: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range list {
		shape := geo.Bbox{XMin: d.cx - d.half, YMin: -d.half, XMax: d.cx + d.half, YMax: d.half}
		row := cache.Row{ID: d.id, Country: "CH", Name: d.name, Subtype: d.subtype, AdminLevel: -1, Bbox: unitBox}
		if err := w.Add(row, wkbBox(shape)); err != nil {
			w.Abort()
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeFallbackCaches(t *testing.T, dir string, list ...division) {
	t.Helper()
	writeCache(t, WorldPath(dir), cache.Row{ID: "country", Country: "CH", Name: "Switzerland", Subtype: "country", Bbox: unitBox})
	writeDivisions(t, DivisionsPath(dir, "CH"), list...)
}

func TestCityFallbackNamesTheCityFromADivision(t *testing.T) {
	dir := t.TempDir()
	writeFallbackCaches(t, dir,
		division{"region", "Big Region", "region", 0, 1},
		division{"county", "Home County", "county", 0, 0.5},
		// 0.003 degrees from the origin, within the default fallback distance.
		division{"macrocounty", "Next County", "macrocounty", 0.005, 0.002})
	for _, tc := range []struct{ name, profile, city, from, decision string }{
		{"state by default", `{}`, "Big Region", geocode.SourceState, ""},
		{"country alone", `{"cityFallback": ["country"]}`, "Switzerland", geocode.SourceCountry, ""},
		{"containing division", `{"cityFallback": ["county", "state"]}`, "Home County", "county", "city fallback"},
		{"nearest division", `{"cityFallback": ["macrocounty", "state"]}`, "Next County", "macrocounty", "city fallback, nearest"},
		{"an unsupplied subtype is skipped", `{"cityFallback": ["localadmin", "county"]}`, "Home County", "county", "city fallback"},
		{"listed order", `{"cityFallback": ["state", "county"]}`, "Big Region", geocode.SourceState, ""},
		{"disabled", `{"cityFallback": []}`, "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profiles, _ := writeProfileCatalog(t, t.TempDir(),
				`{"defaultProfile": {"airports": false}, "countryOverrides": {"CH": `+tc.profile+`}}`)
			p := New(dir, profiles, nil, nil)
			defer p.Close()
			e, err := p.Explain(context.Background(), geocode.Point{})
			if err != nil {
				t.Fatal(err)
			}
			if e.City != "" {
				t.Fatalf("fixture named a city: %+v", e)
			}
			if e.Result.City != tc.city || e.CityFilledFrom != tc.from || e.Result.State != "Big Region" {
				t.Errorf("city %q from %q, state %q; want %q from %q", e.Result.City, e.CityFilledFrom, e.Result.State, tc.city, tc.from)
			}
			if tc.decision != "" {
				if got := candidateNamed(t, e.Divisions, tc.city).Decision; got != tc.decision {
					t.Errorf("decision %q, want %q", got, tc.decision)
				}
			}
		})
	}
}

// County fallback must preserve a nearby locality; preferredSubtypes can override it.
func TestCityFallbackDoesNotPreemptNearerNames(t *testing.T) {
	dir := t.TempDir()
	writeFallbackCaches(t, dir,
		division{"region", "Big Region", "region", 0, 1},
		division{"county", "Home County", "county", 0, 0.5},
		// The locality is 0.003 degrees from the query point.
		division{"locality", "Near City", "locality", 0.005, 0.002})
	for _, tc := range []struct{ name, profile, city, from string }{
		{"nearest locality wins", `{"cityFallback": ["county", "state"]}`, "Near City", ""},
		{"no locality in reach", `{"cityFallback": ["county", "state"], "fallbackDistance": 0}`, "Home County", "county"},
		{"county among the preferred subtypes pre-empts it",
			`{"preferredSubtypes": ["locality", "county"], "cityFallback": ["county", "state"]}`, "Home County", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profiles, _ := writeProfileCatalog(t, t.TempDir(),
				`{"defaultProfile": {"airports": false}, "countryOverrides": {"CH": `+tc.profile+`}}`)
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
			if res.City != tc.city || e.CityFilledFrom != tc.from || e.Result != res {
				t.Errorf("city %q from %q (explain %+v); want %q from %q", res.City, e.CityFilledFrom, e.Result, tc.city, tc.from)
			}
		})
	}
}

func TestCityFallbackReusesSelectedDivisions(t *testing.T) {
	for _, tc := range []struct {
		name, profile, state, country, decision string
		cx, half                                float64
	}{
		{"state", `{}`, "Home County", "Switzerland", "state, city fallback", 0, 0.5},
		{"nearest state", `{}`, "Home County", "Switzerland", "state, nearest, city fallback", 0.005, 0.002},
		{"country", `{"countryFrom":"county"}`, "Other County", "Home County", "country, city fallback", 0, 0.5},
		{"cleared city", `{"preferredSubtypes":["county"],"cityOverrides":[{"from":"Home County","to":""}]}`, "Home County", "Switzerland", "state, city, city fallback", 0, 0.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFallbackCaches(t, dir,
				division{"a", "Home County", "county", tc.cx, tc.half},
				division{"b", "Other County", "county", tc.cx, tc.half})
			profiles, _ := writeProfileCatalog(t, t.TempDir(),
				`{"defaultProfile":{"airports":false,"cityFallback":["county","country"]},"countryOverrides":{"CH":`+tc.profile+`}}`)
			p := New(dir, profiles, nil, nil)
			defer p.Close()
			e, err := p.Explain(context.Background(), geocode.Point{})
			if err != nil {
				t.Fatal(err)
			}
			want := geocode.Result{City: "Home County", State: tc.state, Country: tc.country, Found: true}
			if e.Result != want || e.CityFilledFrom != "county" {
				t.Errorf("result %+v from %q; want %+v from county", e.Result, e.CityFilledFrom, want)
			}
			if got := candidateNamed(t, e.Divisions, "Home County").Decision; got != tc.decision {
				t.Errorf("decision %q; want %q", got, tc.decision)
			}
			res, err := p.Resolve(context.Background(), geocode.Point{})
			if err != nil || res != want {
				t.Errorf("Resolve = %+v, %v; want %+v", res, err, want)
			}
		})
	}
}

func TestCityFallbackSkipsRejectedNames(t *testing.T) {
	dir := t.TempDir()
	writeFallbackCaches(t, dir,
		division{"region", "Bavaria", "region", 0, 1},
		division{"county", "Landkreis Oberallgäu", "county", 0, 0.5})
	for _, tc := range []struct{ name, profile, city, from string }{
		{"county fills", `{"cityFallback": ["county", "state"]}`, "Landkreis Oberallgäu", "county"},
		{"rejected county", `{"cityFallback": ["county", "state"], "rejectNamePrefixes": ["Landkreis "]}`, "Bavaria", geocode.SourceState},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profiles, _ := writeProfileCatalog(t, t.TempDir(),
				`{"defaultProfile": {"airports": false}, "countryOverrides": {"CH": `+tc.profile+`}}`)
			p := New(dir, profiles, nil, nil)
			defer p.Close()
			e, err := p.Explain(context.Background(), geocode.Point{})
			if err != nil {
				t.Fatal(err)
			}
			if e.Result.City != tc.city || e.CityFilledFrom != tc.from {
				t.Errorf("city %q from %q; want %q from %q", e.Result.City, e.CityFilledFrom, tc.city, tc.from)
			}
		})
	}
}

package overture

import (
	"context"
	"strings"
	"testing"

	"github.com/alexzeitgeist/immich-placenames/internal/cache"
	"github.com/alexzeitgeist/immich-placenames/internal/geocode"
)

func TestRejectedByIgnoresCaseAndKeepsTheTrailingSpace(t *testing.T) {
	p := Profile{RejectNamePrefixes: strs("Landkreis ", "Kreis ")}
	for _, c := range []struct{ name, want string }{
		{"Landkreis Oberallgäu", "Landkreis "},
		{"landkreis Tübingen", "Landkreis "},
		{"Kreis Steinfurt", "Kreis "},
		{"Kreischa", ""}, // a municipality, not a district
		{"Oberstdorf", ""},
		{"Landkreis", ""}, // shorter than the prefix
		{"", ""},
	} {
		if got := p.RejectedBy(c.name); got != c.want {
			t.Errorf("%q: got %q, want %q", c.name, got, c.want)
		}
	}
	if got := (Profile{}).RejectedBy("Landkreis Oberallgäu"); got != "" {
		t.Errorf("profile without prefixes rejected %q", got)
	}
}

func TestRejectedByUnicodeCaseFolding(t *testing.T) {
	for _, tc := range []struct {
		prefix, name string
		rejected     bool
	}{
		{"STRAẞE ", "straße Berlin", true},
		{"straße ", "STRAẞE Berlin", true},
		{"ẞ", "ß", true},
		{"ß", "ẞ", true},
		{"Σ ", "ς Athens", true},
		{"Kreis ", "Kreis Steinfurt", true},
		{"STRAẞE ", "straße", false},
		{"straße ", "STRASSE Berlin", false}, // simple folding does not expand ß
		{"straße ", "straßeX Berlin", false},
		{"straße ", "", false},
	} {
		t.Run(tc.prefix+"/"+tc.name, func(t *testing.T) {
			p := Profile{RejectNamePrefixes: strs(tc.prefix)}
			got := p.RejectedBy(tc.name)
			want := ""
			if tc.rejected {
				want = tc.prefix
			}
			if got != want {
				t.Errorf("RejectedBy(%q) = %q, want %q", tc.name, got, want)
			}
		})
	}
}

func TestRejectedNamesLeaveTheChoiceToTheNext(t *testing.T) {
	p := Profile{RejectNamePrefixes: strs("Landkreis ", "Flugplatz ")}
	list := []string{"county", "locality"}
	cs := []Candidate{cand("county", "Landkreis Oberallgäu", 0.9), cand("locality", "Oberstdorf", 0.1)}
	rejectNames(cs, p)
	if got := pick(t, cs, selectName(cs, list, true)); got != "Oberstdorf" {
		t.Errorf("containing: got %s", got)
	}
	if !strings.Contains(cs[0].Decision, `"Landkreis "`) {
		t.Errorf("decision does not name the prefix: %q", cs[0].Decision)
	}
	near := []Candidate{cand("county", "Landkreis Oberallgäu", 0.9, outside), cand("locality", "Oberstdorf", 0.1, outside)}
	rejectNames(near, p)
	if got := pick(t, near, selectNearest(near, list, 0.01)); got != "Oberstdorf" {
		t.Errorf("nearest: got %s", got)
	}
	air := []Candidate{cand("airport", "Flugplatz Oberstdorf", 0.1, class("airfield"))}
	rejectNames(air, p)
	if i := selectAirport(air, geocode.Point{}); i >= 0 {
		t.Errorf("rejected airport selected: %s", air[i].Name)
	}
}

func TestGermanProfileNameRejectionIsOptIn(t *testing.T) {
	dir := t.TempDir()
	writeCache(t, WorldPath(dir), cache.Row{ID: "de", Country: "DE", Name: "Germany", Subtype: "country", Bbox: unitBox})
	writeCache(t, DivisionsPath(dir, "DE"),
		cache.Row{ID: "region", Country: "DE", Name: "Bavaria", Subtype: "region", Bbox: unitBox},
		cache.Row{ID: "county", Country: "DE", Name: "Landkreis Oberallgäu", Subtype: "county", Bbox: unitBox},
		cache.Row{ID: "locality", Country: "DE", Name: "Oberstdorf", Subtype: "locality", Bbox: unitBox},
	)
	for _, tc := range []struct {
		name, catalog, city string
	}{
		{"bundled", `{"defaultProfile": {"airports": false}}`, "Landkreis Oberallgäu"},
		{"opt-in", `{"defaultProfile": {"airports": false}, "countryOverrides": {"DE": {"rejectNamePrefixes": ["Landkreis ", "Kreis "]}}}`, "Oberstdorf"},
		{"inherited", `{"defaultProfile": {"airports": false, "rejectNamePrefixes": ["Landkreis ", "Kreis "]}}`, "Oberstdorf"},
		{"cleared", `{"defaultProfile": {"airports": false, "rejectNamePrefixes": ["Landkreis ", "Kreis "]}, "countryOverrides": {"DE": {"rejectNamePrefixes": []}}}`, "Landkreis Oberallgäu"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profiles, _ := writeProfileCatalog(t, t.TempDir(), tc.catalog)
			p := New(dir, profiles, nil, nil)
			e, err := p.Explain(context.Background(), geocode.Point{})
			p.Close()
			if err != nil {
				t.Fatal(err)
			}
			if e.Result.City != tc.city || e.Result.State != "Bavaria" || e.Result.Country != "Germany" {
				t.Errorf("got %+v, want city %q in Bavaria, Germany", e.Result, tc.city)
			}
			county := candidateNamed(t, e.Divisions, "Landkreis Oberallgäu")
			if rejected := strings.Contains(county.Decision, "name rejected"); rejected != (tc.city == "Oberstdorf") {
				t.Errorf("county decision %q", county.Decision)
			}
		})
	}
}

func TestCountryFromNamesTheCountryAfterADivision(t *testing.T) {
	dir := t.TempDir()
	writeCacheHeader(t, WorldPath(dir), cache.Header{Kind: "test", Code: "GB", Dependencies: true}, geocode.Point{},
		cache.Row{ID: "gb", Country: "GB", Name: "United Kingdom", Subtype: "country", Bbox: unitBox})
	writeCacheHeader(t, DivisionsPath(dir, "GB"), cache.Header{Kind: "test", Code: "GB", Dependencies: true}, geocode.Point{},
		cache.Row{ID: "region", Country: "GB", Name: "Scotland", Subtype: "region", Bbox: unitBox},
		cache.Row{ID: "county", Country: "GB", Name: "City of Edinburgh", Subtype: "county", Bbox: unitBox},
		cache.Row{ID: "locality", Country: "GB", Name: "Old Town", Subtype: "locality", Bbox: unitBox},
	)
	for _, tc := range []struct {
		name, settings           string
		scotlandDecision         string
		state, country, replaced string
	}{
		{name: "unset", state: "Scotland", country: "United Kingdom", scotlandDecision: "state"},
		{name: "region", settings: `"countryFrom": "region",`, state: "City of Edinburgh", country: "Scotland", replaced: "United Kingdom", scotlandDecision: "country"},
		{name: "rejected country division", settings: `"countryFrom": "region", "rejectNamePrefixes": ["Scot"],`, state: "City of Edinburgh", country: "United Kingdom", scotlandDecision: `name rejected, prefix "Scot"`},
		{name: "absent subtype", settings: `"countryFrom": "dependency",`, state: "Scotland", country: "United Kingdom", scotlandDecision: "state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profiles, _ := writeProfileCatalog(t, t.TempDir(), `{
  "defaultProfile": {"airports": false},
  "countryOverrides": {"GB": {`+tc.settings+` "preferredSubtypes": ["locality"]}}
}`)
			p := New(dir, profiles, nil, nil)
			e, err := p.Explain(context.Background(), geocode.Point{})
			p.Close()
			if err != nil {
				t.Fatal(err)
			}
			if e.Result.City != "Old Town" || e.Result.State != tc.state || e.Result.Country != tc.country {
				t.Errorf("got %+v, want Old Town, %q, %q", e.Result, tc.state, tc.country)
			}
			if e.CountryReplaced != tc.replaced {
				t.Errorf("replaced country: got %q, want %q", e.CountryReplaced, tc.replaced)
			}
			if _, loaded := e.Releases["divisions/GB"]; e.Code != "GB" || !loaded {
				t.Errorf("country code %q, releases %v", e.Code, e.Releases)
			}
			if got := candidateNamed(t, e.Divisions, "Scotland").Decision; got != tc.scotlandDecision {
				t.Errorf("Scotland decision: got %q, want %q", got, tc.scotlandDecision)
			}
		})
	}
}

func TestCountryFromKeepsCountryWhenDivisionUnnamed(t *testing.T) {
	dir := t.TempDir()
	writeCache(t, WorldPath(dir), cache.Row{ID: "gb", Country: "GB", Name: "United Kingdom", Subtype: "country", Bbox: unitBox})
	writeCache(t, DivisionsPath(dir, "GB"),
		cache.Row{ID: "region", Country: "GB", Subtype: "region", Bbox: unitBox},
		cache.Row{ID: "locality", Country: "GB", Name: "Old Town", Subtype: "locality", Bbox: unitBox},
	)
	profiles, _ := writeProfileCatalog(t, t.TempDir(),
		`{"countryOverrides": {"GB": {"airports": false, "countryFrom": "region"}}}`)
	p := New(dir, profiles, nil, nil)
	defer p.Close()
	e, err := p.Explain(context.Background(), geocode.Point{})
	if err != nil {
		t.Fatal(err)
	}
	if e.Result.Country != "United Kingdom" || e.CountryReplaced != "" || !e.Result.Writable() {
		t.Errorf("unnamed region changed country: result %+v, replaced %q", e.Result, e.CountryReplaced)
	}
}

func TestRejectedNamesReachThePointFallback(t *testing.T) {
	dir := t.TempDir()
	writeCache(t, WorldPath(dir), cache.Row{ID: "country", Country: "CH", Name: "Switzerland", Subtype: "country", Bbox: unitBox})
	writeAreas(t, DivisionsPath(dir, "CH"), area{"region", "Big Region", "region", 1})
	// 0.001 degrees of longitude is 111 m at the equator.
	writeLabels(t, PointsPath(dir, "CH"),
		label{"district", "Landkreis Oberallgäu", "locality", 0.001, 0},
		label{"town", "Oberstdorf", "locality", 0.002, 0})
	for _, tc := range []struct{ name, reject, city string }{
		{"nearest label", "", "Landkreis Oberallgäu"},
		{"rejected label", `"rejectNamePrefixes": ["Landkreis "],`, "Oberstdorf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profiles, _ := writeProfileCatalog(t, t.TempDir(),
				`{"countryOverrides": {"CH": {"airports": false, "pointFallback": true, `+tc.reject+` "preferredSubtypes": ["locality"]}}}`)
			p := New(dir, profiles, nil, nil)
			e, err := p.Explain(context.Background(), geocode.Point{})
			p.Close()
			if err != nil {
				t.Fatal(err)
			}
			if e.Result.City != tc.city {
				t.Errorf("city %q, want %q", e.Result.City, tc.city)
			}
			district := candidateNamed(t, e.Points, "Landkreis Oberallgäu")
			want := "city, point"
			if tc.reject != "" {
				want = `name rejected, prefix "Landkreis "`
			}
			if district.Decision != want {
				t.Errorf("label decision %q, want %q", district.Decision, want)
			}
		})
	}
}

func candidateNamed(t *testing.T, cands []Candidate, name string) Candidate {
	t.Helper()
	for _, c := range cands {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no candidate named %q in %+v", name, cands)
	return Candidate{}
}

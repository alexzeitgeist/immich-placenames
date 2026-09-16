package overture

import (
	"context"
	"strings"
	"testing"

	"github.com/alexzeitgeist/immich-placenames/internal/cache"
	"github.com/alexzeitgeist/immich-placenames/internal/geocode"
)

func TestRejectedByMatchesAnywhereIgnoringCase(t *testing.T) {
	district := `kreis($|\s)` // the bundled German pattern
	p := Profile{RejectNamePatterns: pats(district, "^Flugplatz ")}
	for _, c := range []struct{ name, want string }{
		{"Landkreis Oberallgäu", district},
		{"landkreis Tübingen", district},
		{"Kreis Steinfurt", district},
		{"Wetteraukreis", district},
		{"Rhein-Kreis Neuss", district},
		{"Flugplatz Oberstdorf", "^Flugplatz "},
		{"Kreischa", ""}, // a municipality, not a district
		{"Kreishafen", ""},
		{"Kurpfalzkreisel", ""},
		{"Oberstdorf", ""},
		{"", ""},
	} {
		if got := p.RejectedBy(c.name, RoleCity); got != c.want {
			t.Errorf("%q: got %q, want %q", c.name, got, c.want)
		}
	}
	if got := (Profile{}).RejectedBy("Landkreis Oberallgäu", RoleCity); got != "" {
		t.Errorf("profile without patterns rejected %q", got)
	}
}

func TestRejectedByUnicodeCaseFolding(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		rejected      bool
	}{
		{"STRAẞE ", "straße Berlin", true},
		{"straße ", "STRAẞE Berlin", true},
		{"ẞ", "ß", true},
		{"ß", "ẞ", true},
		{"Σ ", "ς Athens", true},
		{"(?-i)Kreis ", "Kreis Steinfurt", true},
		{"(?-i)Kreis ", "kreis Steinfurt", false},
		{"Kreis ", "Kreis Steinfurt", true},
		{"STRAẞE ", "straße", false},
		{"straße ", "STRASSE Berlin", false}, // simple folding does not expand ß
		{"straße ", "straßeX Berlin", false},
		{"straße ", "", false},
	} {
		t.Run(tc.pattern+"/"+tc.name, func(t *testing.T) {
			p := Profile{RejectNamePatterns: pats(tc.pattern)}
			got := p.RejectedBy(tc.name, RoleCity)
			want := ""
			if tc.rejected {
				want = tc.pattern
			}
			if got != want {
				t.Errorf("RejectedBy(%q) = %q, want %q", tc.name, got, want)
			}
		})
	}
}

func TestRejectedNamesLeaveTheChoiceToTheNext(t *testing.T) {
	p := Profile{RejectNamePatterns: pats("^Landkreis ", "^Flugplatz ")}
	list := []string{"county", "locality"}
	cs := []Candidate{cand("county", "Landkreis Oberallgäu", 0.9), cand("locality", "Oberstdorf", 0.1)}
	rejectNames(cs, p)
	if got := pick(t, cs, selectName(cs, list, true, RoleCity)); got != "Oberstdorf" {
		t.Errorf("containing: got %s", got)
	}
	noteRejections(cs)
	if !strings.Contains(cs[0].Decision, `"^Landkreis "`) {
		t.Errorf("decision does not name the pattern: %q", cs[0].Decision)
	}
	near := []Candidate{cand("county", "Landkreis Oberallgäu", 0.9, outside), cand("locality", "Oberstdorf", 0.1, outside)}
	rejectNames(near, p)
	if got := pick(t, near, selectNearest(near, list, 0.01, RoleCity)); got != "Oberstdorf" {
		t.Errorf("nearest: got %s", got)
	}
	air := []Candidate{cand("airport", "Flugplatz Oberstdorf", 0.1, class("airfield"))}
	rejectNames(air, p)
	if i := selectAirport(air, geocode.Point{}); i >= 0 {
		t.Errorf("rejected airport selected: %s", air[i].Name)
	}
}

func TestCityRoleGovernsAirportsAndLabels(t *testing.T) {
	for _, tc := range []struct{ role, want, airportDecision, pointDecision string }{
		{RoleCity, "", `name rejected for city, pattern "^Flughafen "`, `name rejected for city, pattern "^Flughafen "`},
		{RoleState, "Flughafen Zürich", `airport; name rejected for state, pattern "^Flughafen "`, `city, point; name rejected for state, pattern "^Flughafen "`},
	} {
		t.Run(tc.role, func(t *testing.T) {
			p := Profile{RejectNamePatterns: scoped("^Flughafen ", tc.role)}
			air := []Candidate{cand("airport", "Flughafen Zürich", 0.1, class("international_airport"))}
			rejectNames(air, p)
			ai := selectAirport(air, geocode.Point{})
			if got := pick(t, air, ai); got != tc.want {
				t.Errorf("airport: got %q, want %q", got, tc.want)
			}
			decide(air, ai, "airport", fillsCity)
			noteRejections(air)
			if got := air[0].Decision; got != tc.airportDecision {
				t.Errorf("airport decision: got %q, want %q", got, tc.airportDecision)
			}
			labels := []Candidate{cand("locality", "Flughafen Zürich", 0.1)}
			rejectNames(labels, p)
			pi := selectPoint(labels, []string{"locality"}, DefaultPointDistance)
			if got := pick(t, labels, pi); got != tc.want {
				t.Errorf("label: got %q, want %q", got, tc.want)
			}
			decidePoints(labels, pi, []string{"locality"}, DefaultPointDistance)
			noteRejections(labels)
			if got := labels[0].Decision; got != tc.pointDecision {
				t.Errorf("point decision: got %q, want %q", got, tc.pointDecision)
			}
		})
	}
}

func TestProfileTextMarksScopedPatterns(t *testing.T) {
	p := Profile{RejectNamePatterns: &[]RejectPattern{
		{Pattern: `kreis($|\s)`},
		{Pattern: "^Gespanschaft ", Roles: []string{RoleCity}},
		{Pattern: "^Region ", Roles: []string{RoleState, RoleCity}},
	}}.normalize()
	want := ` reject=["kreis($|\\s)" "^Gespanschaft "@city "^Region "@city,state]`
	if !strings.Contains(p.String(), want) {
		t.Errorf("%s does not contain %s", p.String(), want)
	}
}

func TestMixedPatternsKeepTheFirstMatchForEachRole(t *testing.T) {
	profiles, _ := writeProfileCatalog(t, t.TempDir(), `{"defaultProfile": {
  "rejectNamePatterns": [
    {"pattern": "^Region ", "roles": [" STATE ", "city", "state"]},
    "Region",
    {"pattern": "Example$", "roles": ["country"]}
  ]
}}`)
	p := profiles.Default()
	cs := []Candidate{cand("county", "Region Example", 0.1)}
	rejectNames(cs, p)
	for _, tc := range []struct{ role, pattern string }{
		{RoleCity, "^Region "},
		{RoleState, "^Region "},
		{RoleCountry, "Region"},
	} {
		if got := cs[0].Rejected[tc.role]; got != tc.pattern {
			t.Errorf("%s: got %q, want %q", tc.role, got, tc.pattern)
		}
		if i := selectName(cs, []string{"county"}, false, tc.role); i != -1 {
			t.Errorf("rejected candidate selected for %s", tc.role)
		}
	}
	decide(cs, -1, "", allRoles, []string{"county"})
	noteRejections(cs)
	want := `name rejected for city,state, pattern "^Region "; name rejected for country, pattern "Region"`
	if got := cs[0].Decision; got != want {
		t.Errorf("decision %q, want %q", got, want)
	}
}

func TestRejectPatternErrorsIdentifyInvalidFields(t *testing.T) {
	for _, tc := range []struct{ entry, want string }{
		{`{"pattern": "kreis", "roles": ["city"], "typo": true}`, `unknown field "typo"`},
		{`{"pattern": "kreis", "roles": "city"}`, "cannot unmarshal string"},
	} {
		var c Catalog
		err := decode([]byte(`{"defaultProfile":{"rejectNamePatterns":[`+tc.entry+`]}}`), &c)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want error containing %q", tc.entry, err, tc.want)
		}
	}
}

func TestGermanProfileExpandedRejections(t *testing.T) {
	profiles, err := LoadProfiles("")
	if err != nil {
		t.Fatal(err)
	}
	p := profiles.Profile("DE")
	for _, tc := range []struct{ rejected, city string }{
		{"Städteregion Aachen", "Aachen"},
		{"Hannover Region", "Hannover"},
		{"Lake Constance district", "Salem"},
		{"Regionalverband Saarbrücken", "Saarbrücken"},
	} {
		t.Run(tc.rejected, func(t *testing.T) {
			cs := []Candidate{
				cand("county", tc.rejected, 0.9),
				cand("locality", tc.city, 0.1),
			}
			rejectNames(cs, p)
			if got := pick(t, cs, selectName(cs, p.PreferredSubtypes, p.TieBreakMode == TieBreakLargest, RoleCity)); got != tc.city {
				t.Errorf("got %q, want %q", got, tc.city)
			}
			noteRejections(cs)
			if !strings.HasPrefix(cs[0].Decision, "name rejected, pattern ") {
				t.Errorf("district decision %q, want name rejection", cs[0].Decision)
			}
			if cs[1].Decision != "" {
				t.Errorf("municipality %q rejected: %s", tc.city, cs[1].Decision)
			}
		})
	}
}

func TestRejectedAirportLeavesTheCityToTheDivisions(t *testing.T) {
	dir := t.TempDir()
	writeCache(t, WorldPath(dir), cache.Row{ID: "country", Country: "CH", Name: "Switzerland", Subtype: "country", Bbox: unitBox})
	writeCache(t, DivisionsPath(dir, "CH"), cache.Row{ID: "locality", Country: "CH", Name: "Kloten", Subtype: "locality", Bbox: unitBox})
	writeCache(t, AirportsPath(dir), cache.Row{ID: "airport", Name: "Flughafen Zürich", Subtype: "airport", Class: "international_airport", Bbox: unitBox})
	for _, tc := range []struct{ name, roles, city, decision string }{
		{"every role", `["city", "state", "country"]`, "Kloten", `name rejected, pattern "^Flughafen "`},
		{"the city role", `["city"]`, "Kloten", `name rejected for city, pattern "^Flughafen "`},
		{"the state role", `["state"]`, "Flughafen Zürich", `airport; name rejected for state, pattern "^Flughafen "`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profiles, _ := writeProfileCatalog(t, t.TempDir(),
				`{"countryOverrides": {"CH": {"rejectNamePatterns": [{"pattern": "^Flughafen ", "roles": `+tc.roles+`}]}}}`)
			p := New(dir, profiles, nil, nil)
			e, err := p.Explain(context.Background(), geocode.Point{})
			p.Close()
			if err != nil {
				t.Fatal(err)
			}
			if e.Result.City != tc.city {
				t.Errorf("city %q, want %q", e.Result.City, tc.city)
			}
			if got := candidateNamed(t, e.Airports, "Flughafen Zürich").Decision; got != tc.decision {
				t.Errorf("airport decision %q, want %q", got, tc.decision)
			}
		})
	}
}

func TestGermanProfileRejectsDistrictNames(t *testing.T) {
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
		{"bundled", `{"defaultProfile": {"airports": false}}`, "Oberstdorf"},
		{"cleared by the country", `{"defaultProfile": {"airports": false}, "countryOverrides": {"DE": {"rejectNamePatterns": []}}}`, "Landkreis Oberallgäu"},
		{"cleared by the user default", `{"defaultProfile": {"airports": false, "rejectNamePatterns": []}}`, "Landkreis Oberallgäu"},
		{"replaced, not merged", `{"defaultProfile": {"airports": false}, "countryOverrides": {"DE": {"rejectNamePatterns": ["^kreis "]}}}`, "Landkreis Oberallgäu"},
		{"inherited from the user default", `{"defaultProfile": {"airports": false, "rejectNamePatterns": ["kreis "]}}`, "Oberstdorf"},
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

func TestRoleScopedRejectionKeepsTheOtherRole(t *testing.T) {
	dir := t.TempDir()
	writeCache(t, WorldPath(dir), cache.Row{ID: "country", Country: "HR", Name: "Croatia", Subtype: "country", Bbox: unitBox})
	writeCache(t, DivisionsPath(dir, "HR"),
		cache.Row{ID: "county", Country: "HR", Name: "Gespanschaft Dubrovnik-Neretva", Subtype: "county", Bbox: unitBox})
	for _, tc := range []struct{ name, fallback, city, from string }{
		{"cleared fallback leaves the city empty", `, "cityFallback": []`, "", ""},
		{"the default fallback copies the state", "", "Gespanschaft Dubrovnik-Neretva", geocode.SourceState},
		{"a country fallback copies the country", `, "cityFallback": ["country"]`, "Croatia", geocode.SourceCountry},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profiles, _ := writeProfileCatalog(t, t.TempDir(), `{"countryOverrides": {"HR": {
  "airports": false, "preferredSubtypes": ["county"], "stateSubtypes": ["county"],
  "rejectNamePatterns": [{"pattern": "^Gespanschaft ", "roles": ["city"]}]`+tc.fallback+`}}}`)
			p := New(dir, profiles, nil, nil)
			e, err := p.Explain(context.Background(), geocode.Point{})
			p.Close()
			if err != nil {
				t.Fatal(err)
			}
			if e.City != "" {
				t.Errorf("selection used a rejected name for the city: %q", e.City)
			}
			if e.Result.State != "Gespanschaft Dubrovnik-Neretva" {
				t.Errorf("state %q, want the county", e.Result.State)
			}
			if e.Result.City != tc.city || e.CityFilledFrom != tc.from {
				t.Errorf("city %q from %q; want %q from %q", e.Result.City, e.CityFilledFrom, tc.city, tc.from)
			}
			want := `state; name rejected for city, pattern "^Gespanschaft "`
			if got := candidateNamed(t, e.Divisions, "Gespanschaft Dubrovnik-Neretva").Decision; got != want {
				t.Errorf("decision %q, want %q", got, want)
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
		{name: "rejected country division", settings: `"countryFrom": "region", "rejectNamePatterns": ["^Scot"],`, state: "City of Edinburgh", country: "United Kingdom", scotlandDecision: `name rejected, pattern "^Scot"`},
		{name: "rejected for the country role", settings: `"countryFrom": "region", "rejectNamePatterns": [{"pattern": "^Scot", "roles": ["country"]}],`, state: "Scotland", country: "United Kingdom", scotlandDecision: `state; name rejected for country, pattern "^Scot"`},
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
	for _, tc := range []struct{ name, reject, city, decision string }{
		{"nearest label", "", "Landkreis Oberallgäu", "city, point"},
		{"rejected label", `"rejectNamePatterns": ["^Landkreis "],`, "Oberstdorf", `name rejected, pattern "^Landkreis "`},
		{"rejected for the city role", `"rejectNamePatterns": [{"pattern": "^Landkreis ", "roles": ["city"]}],`, "Oberstdorf", `name rejected for city, pattern "^Landkreis "`},
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
			if district.Decision != tc.decision {
				t.Errorf("label decision %q, want %q", district.Decision, tc.decision)
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

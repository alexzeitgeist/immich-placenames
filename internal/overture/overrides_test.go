package overture

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/alexzeitgeist/immich-placenames/internal/cache"
	"github.com/alexzeitgeist/immich-placenames/internal/geocode"
)

func writeProfileCatalog(t *testing.T, dir, body string) (*Profiles, string) {
	t.Helper()
	path := filepath.Join(dir, "profiles.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	profiles, err := LoadProfiles(path)
	if err != nil {
		t.Fatal(err)
	}
	return profiles, path
}

func writeProviderBaseCaches(t *testing.T, dir string) {
	t.Helper()
	writeCache(t, WorldPath(dir), cache.Row{
		ID: "country", Country: "CH", Name: "Switzerland", Subtype: "country", Bbox: unitBox,
	})
	writeCache(t, DivisionsPath(dir, "CH"), cache.Row{
		ID: "city", Country: "CH", Name: "Real City", Subtype: "locality", Bbox: unitBox,
	})
}

func TestProviderProfileOverrideIsLastAndIsolated(t *testing.T) {
	dir := t.TempDir()
	writeCache(t, WorldPath(dir), cache.Row{
		ID: "country", Country: "CH", Name: "Switzerland", Subtype: "country", Bbox: unitBox,
	})
	writeCacheAt(t, DivisionsPath(dir, "CH"), geocode.Point{Lon: 0.005}, cache.Row{
		ID: "city", Country: "CH", Name: "Real City", Subtype: "locality", Bbox: unitBox,
	})
	writeCache(t, AirportsPath(dir), cache.Row{
		ID: "airport", Name: "Real Airport", Subtype: "airport", Class: "airport", Bbox: unitBox,
	})
	profiles, _ := writeProfileCatalog(t, dir, `{
  "defaultProfile": {
    "preferredSubtypes": ["locality"],
    "tieBreakMode": "largest-area",
    "fallbackDistance": 0,
    "airports": false,
    "stateSubtypes": ["region"],
    "language": "en"
  },
  "countryOverrides": {
    "CH": {"fallbackDistance": 0.01, "airports": true}
  }
}`)

	base := profiles.Profile("CH")
	if *base.FallbackDistance != 0.01 || !*base.Airports {
		t.Fatalf("catalog profile: %v", base)
	}
	override := Profile{FallbackDistance: f64(0), Airports: boolp(false)}
	overrideBefore := override

	p := New(dir, profiles, nil, nil)
	p.Overrides = override
	e, err := p.Explain(context.Background(), geocode.Point{})
	if err != nil {
		p.Close()
		t.Fatal(err)
	}

	want := base.apply(override).normalize()
	if !reflect.DeepEqual(e.Profile, want) {
		t.Errorf("effective profile: got %v, want %v", e.Profile, want)
	}
	if e.Airport != "" || len(e.Airports) != 0 || e.Result.City != "" || e.Result.State != "" {
		t.Errorf("override did not disable airport and nearest selection: explanation=%+v", e)
	}
	if got := e.Result.WithFallback(); got.City != "Switzerland" || got.State != "" || got.Country != "Switzerland" || !got.Found {
		t.Errorf("geocode fallback changed: got %+v", got)
	}
	if got := e.Profile.String(); got != want.String() || got == base.String() {
		t.Errorf("profile text: got %q, want %q", got, want.String())
	}
	encoded, err := json.Marshal(e)
	if err != nil {
		p.Close()
		t.Fatal(err)
	}
	var decoded struct {
		Profile Profile `json:"profile"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		p.Close()
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Profile, e.Profile) {
		t.Errorf("JSON profile: got %v, want %v", decoded.Profile, e.Profile)
	}
	if !reflect.DeepEqual(override, overrideBefore) {
		t.Errorf("provider mutated override: got %v, want %v", override, overrideBefore)
	}
	p.Close()

	if got := profiles.Profile("CH"); !reflect.DeepEqual(got, base) {
		t.Errorf("provider mutated catalog: got %v, want %v", got, base)
	}

	without := New(dir, profiles, nil, nil)
	withoutExplanation, err := without.Explain(context.Background(), geocode.Point{})
	without.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(withoutExplanation.Profile, base) || withoutExplanation.Airport != "Real Airport" {
		t.Errorf("provider without override ignored catalog: profile=%v airport=%q", withoutExplanation.Profile, withoutExplanation.Airport)
	}
}

func TestProviderOverrideAirportsControlsFetch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value bool
		calls int
		city  string
	}{
		{name: "disabled", value: false, calls: 0, city: "Real City"},
		{name: "enabled", value: true, calls: 1, city: "Real Airport"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeProviderBaseCaches(t, dir)
			profiles, _ := writeProfileCatalog(t, dir, `{"defaultProfile": {"airports": false}}`)
			fetcher := &airportFetcher{t: t}
			p := New(dir, profiles, fetcher, nil)
			p.Overrides = Profile{Airports: boolp(tc.value)}
			res, err := p.Resolve(context.Background(), geocode.Point{})
			p.Close()
			if err != nil {
				t.Fatal(err)
			}
			if res.City != tc.city || fetcher.calls != tc.calls {
				t.Errorf("got city %q after %d fetches; want %q after %d", res.City, fetcher.calls, tc.city, tc.calls)
			}
		})
	}
}

func TestProviderOverrideFallbackDistanceControlsBothNearestNames(t *testing.T) {
	dir := t.TempDir()
	writeCache(t, WorldPath(dir), cache.Row{
		ID: "country", Country: "CH", Name: "Switzerland", Subtype: "country", Bbox: unitBox,
	})
	near := geocode.Point{Lon: 0.005}
	writeCacheAt(t, DivisionsPath(dir, "CH"), near,
		cache.Row{ID: "state", Country: "CH", Name: "Near State", Subtype: "region", Bbox: unitBox},
		cache.Row{ID: "city", Country: "CH", Name: "Near City", Subtype: "locality", Bbox: unitBox},
	)
	profiles, _ := writeProfileCatalog(t, dir, `{
  "defaultProfile": {"fallbackDistance": 0, "airports": false},
  "countryOverrides": {"CH": {"fallbackDistance": 0}}
}`)

	for _, tc := range []struct {
		name             string
		bound            float64
		city, state      string
		fallbackCity     string
		fallbackExpected bool
	}{
		{name: "zero", bound: 0, fallbackCity: "Switzerland"},
		{name: "below-distance", bound: 0.004, fallbackCity: "Switzerland"},
		{name: "above-distance", bound: 0.006, city: "Near City", state: "Near State", fallbackCity: "Near City", fallbackExpected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := New(dir, profiles, nil, nil)
			p.Overrides = Profile{FallbackDistance: f64(tc.bound)}
			e, err := p.Explain(context.Background(), geocode.Point{})
			p.Close()
			if err != nil {
				t.Fatal(err)
			}
			if e.City != tc.city || e.State != tc.state {
				t.Errorf("nearest names: got city=%q state=%q, want city=%q state=%q", e.City, e.State, tc.city, tc.state)
			}
			if e.Result.City != tc.city || e.Result.State != tc.state {
				t.Errorf("result names: got %+v", e.Result)
			}
			if got := e.Result.WithFallback(); got.City != tc.fallbackCity || got.State != tc.state || !got.Found {
				t.Errorf("WithFallback: got %+v", got)
			}
			if got := *e.Profile.FallbackDistance; got != tc.bound {
				t.Errorf("effective fallback distance: got %g, want %g", got, tc.bound)
			}
			if tc.fallbackExpected && (e.Divisions[0].Decision == "" || e.Divisions[1].Decision == "") {
				t.Errorf("nearest winners lack decisions: %+v", e.Divisions)
			}
		})
	}
}

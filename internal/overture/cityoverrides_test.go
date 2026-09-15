package overture

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/alexzeitgeist/immich-placenames/internal/cache"
	"github.com/alexzeitgeist/immich-placenames/internal/geocode"
)

func TestProfileOverrideMatch(t *testing.T) {
	p := Profile{CityOverrides: []CityOverride{
		{From: " Mörtvik ", To: " Skogås ", State: " Stockholm County "},
		{From: "Mörtvik", To: "Anywhere"},
		{From: "Larsboda", To: ""},
	}}.normalize()
	for _, tc := range []struct {
		name, city, state, want string
		found                   bool
	}{
		{name: "state qualifier", city: "Mörtvik", state: "Stockholm County", want: "Skogås", found: true},
		{name: "case folded", city: "MÖRTVIK", state: "stockholm county", want: "Skogås", found: true},
		{name: "another state takes the unqualified entry", city: "Mörtvik", state: "Skåne County", want: "Anywhere", found: true},
		{name: "cleared", city: "Larsboda", state: "Stockholm County", want: "", found: true},
		{name: "no entry", city: "Farsta", state: "Stockholm County"},
		{name: "no city", state: "Stockholm County"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := p.Override(tc.city, tc.state)
			if (i >= 0) != tc.found {
				t.Fatalf("index %d, want found=%t", i, tc.found)
			}
			if tc.found && p.CityOverrides[i].To != tc.want {
				t.Errorf("rewrote to %q, want %q", p.CityOverrides[i].To, tc.want)
			}
		})
	}
	if got := (Profile{CityOverrides: []CityOverride{{From: "  ", To: "x"}}}).normalize().CityOverrides; got != nil {
		t.Errorf("blank entry kept: %+v", got)
	}
}

// A country's override list replaces the default list.
func TestPointAndOverrideMerge(t *testing.T) {
	dir := t.TempDir()
	user := filepath.Join(dir, "profiles.json")
	os.WriteFile(user, []byte(`{
	  "defaultProfile": {"pointDistance": 800, "cityOverrides": [{"from": "Everywhere", "to": "Default"}]},
	  "countryOverrides": {
	    "SE": {"pointFallback": true, "cityOverrides": [{"from": " Mörtvik ", "to": "Skogås", "state": "Stockholm County"}]},
	    "CH": {"pointFallback": false, "pointDistance": 0}
	  }}`), 0o644)
	p, err := LoadProfiles(user)
	if err != nil {
		t.Fatal(err)
	}
	se := p.Profile("SE")
	if !*se.PointFallback || *se.PointDistance != 800 {
		t.Errorf("SE: %v", se)
	}
	if want := []CityOverride{{From: "Mörtvik", To: "Skogås", State: "Stockholm County"}}; !reflect.DeepEqual(se.CityOverrides, want) {
		t.Errorf("SE overrides: got %+v, want %+v", se.CityOverrides, want)
	}
	if ch := p.Profile("CH"); *ch.PointFallback || *ch.PointDistance != 0 || len(ch.CityOverrides) != 1 {
		t.Errorf("CH: %v", ch)
	}
	if def := p.Default(); *def.PointFallback || *def.PointDistance != 800 {
		t.Errorf("default: %v", def)
	}
	if bundled, _ := LoadProfiles(""); *bundled.Default().PointFallback || *bundled.Default().PointDistance != DefaultPointDistance {
		t.Errorf("bundled default: %v", bundled.Default())
	}
	if got := se.String(); got != "[locality borough localadmin macrohood neighborhood microhood] smallest-area"+
		" state=[region macroregion county macrocounty dependency] fallback=0.01 airports=true points=true@800m"+
		" language=en,primary cityOverrides=1" {
		t.Errorf("profile text: %q", got)
	}
	for name, body := range map[string]string{
		"negative distance": `{"defaultProfile": {"pointDistance": -1}}`,
		"blank from":        `{"countryOverrides": {"SE": {"cityOverrides": [{"from": " ", "to": "Skogås"}]}}}`,
		"missing from":      `{"countryOverrides": {"SE": {"cityOverrides": [{"to": "Skogås"}]}}}`,
		"unknown key":       `{"countryOverrides": {"SE": {"cityOverrides": [{"from": "a", "to": "b", "country": "SE"}]}}}`,
		"point typo":        `{"defaultProfile": {"pointFallbck": true}}`,
	} {
		os.WriteFile(user, []byte(body), 0o644)
		if _, err := LoadProfiles(user); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// Issue 17: city overrides apply after airport replacement.
func TestCityOverrideRewritesTheCity(t *testing.T) {
	dir := t.TempDir()
	writeCache(t, WorldPath(dir), cache.Row{ID: "country", Country: "CH", Name: "Switzerland", Subtype: "country", Bbox: unitBox})
	writeCache(t, DivisionsPath(dir, "CH"),
		cache.Row{ID: "region", Country: "CH", Name: "Real Region", Subtype: "region", Bbox: unitBox},
		cache.Row{ID: "city", Country: "CH", Name: "Mörtvik", Subtype: "locality", Bbox: unitBox})
	writeCache(t, AirportsPath(dir), cache.Row{ID: "airport", Name: "Real Airport", Subtype: "airport", Class: "airport", Bbox: unitBox})
	for _, tc := range []struct {
		name, profile, city, overridden string
	}{
		{"no overrides", `{"airports": false}`, "Mörtvik", ""},
		{"rewrites the city", `{"airports": false, "cityOverrides": [{"from": "mörtvik", "to": "Skogås"}]}`, "Skogås", "Mörtvik"},
		{"state matches", `{"airports": false, "cityOverrides": [{"from": "Mörtvik", "to": "Skogås", "state": "Real Region"}]}`, "Skogås", "Mörtvik"},
		{"state differs", `{"airports": false, "cityOverrides": [{"from": "Mörtvik", "to": "Skogås", "state": "Elsewhere"}]}`, "Mörtvik", ""},
		{"first match wins", `{"airports": false, "cityOverrides": [{"from": "Mörtvik", "to": "First"}, {"from": "Mörtvik", "to": "Second"}]}`, "First", "Mörtvik"},
		{"clears the city", `{"airports": false, "cityOverrides": [{"from": "Mörtvik", "to": ""}]}`, "", "Mörtvik"},
		{"rewrites the airport", `{"cityOverrides": [{"from": "Real Airport", "to": "Arlanda"}]}`, "Arlanda", "Real Airport"},
		{"airport hid the city", `{"cityOverrides": [{"from": "Mörtvik", "to": "Skogås"}]}`, "Real Airport", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profiles, _ := writeProfileCatalog(t, dir, `{"countryOverrides": {"CH": `+tc.profile+`}}`)
			p := New(dir, profiles, nil, nil)
			defer p.Close()
			e, err := p.Explain(context.Background(), geocode.Point{})
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			if tc.overridden != "" {
				count = 1
			}
			if e.Result.City != tc.city || e.Overridden != tc.overridden || p.Overridden() != count {
				t.Errorf("city %q overridden %q count %d; want %q, %q, %d",
					e.Result.City, e.Overridden, p.Overridden(), tc.city, tc.overridden, count)
			}
			if tc.city == "" && e.Result.WithFallback().City != "Real Region" {
				t.Errorf("cleared city ignores the state fallback: %+v", e.Result.WithFallback())
			}
		})
	}
}

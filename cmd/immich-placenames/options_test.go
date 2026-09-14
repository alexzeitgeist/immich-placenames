package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexzeitgeist/immich-placenames/internal/cache"
	"github.com/alexzeitgeist/immich-placenames/internal/geo"
	"github.com/alexzeitgeist/immich-placenames/internal/geocode"
	"github.com/alexzeitgeist/immich-placenames/internal/overture"
)

func TestVerbositySharedByCommands(t *testing.T) {
	a := quiet(t.TempDir())
	var logs, out strings.Builder
	a.errOut, a.out = &logs, &out
	for _, args := range [][]string{{"version"}, {"version", "-v"}, {"version", "-v=false"}, {"version"}} {
		logs.Reset()
		out.Reset()
		if err := a.call(args...); err != nil {
			t.Fatal(err)
		}
		a.log.Debug("command debug")
		p, err := a.provider()
		if err != nil {
			t.Fatal(err)
		}
		p.Log.Debug("provider debug")
		a.fetcher("").c.Log.Debug("fetch debug")
		p.Close()
		want := len(args) == 2 && args[1] == "-v"
		if strings.Contains(logs.String(), "command debug") != want || strings.Contains(logs.String(), "provider debug") != want || strings.Contains(logs.String(), "fetch debug") != want {
			t.Fatalf("%v: logs %q", args, logs.String())
		}
		if strings.Contains(out.String(), "DEBUG") {
			t.Fatal("logs leaked into stdout")
		}
	}
	for _, c := range commands {
		logs.Reset()
		a.call(c.name, "-h")
		if !strings.Contains(logs.String(), "-v") {
			t.Errorf("%s help lacks -v", c.name)
		}
	}
}

func TestNewFlagsValidateBeforeWork(t *testing.T) {
	a := quiet(t.TempDir())
	for _, args := range [][]string{
		{"lookup", "-fallback-distance", "-1", "0", "0"},
		{"lookup", "-fallback-distance", "NaN", "0", "0"},
		{"lookup", "-fallback-distance", "Inf", "0", "0"},
		{"lookup", "-fallback-distance", "-Inf", "0", "0"},
		{"lookup", "-fallback-distance", "abc", "0", "0"},
		{"lookup", "-airports=maybe", "0", "0"},
		{"run", "-page-size", "-1"},
		{"run", "-airports=false"},
		{"run", "-fallback-distance", "0"},
		{"lookup", "-page-size", "1", "0", "0"},
	} {
		if err := a.call(args...); !isUsage(err) {
			t.Errorf("%v: %v", args, err)
		}
	}
	entries, err := os.ReadDir(a.data)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid input created data: %v, %v", entries, err)
	}
}

func lookupCaches(t *testing.T, dir string) {
	t.Helper()
	box := geo.Bbox{XMin: -1, YMin: -1, XMax: 1, YMax: 1}
	for _, f := range []struct {
		path string
		lon  float64
		rows []cache.Row
	}{
		{overture.WorldPath(dir), 0, []cache.Row{{ID: "country", Country: "CH", Name: "Country", Subtype: "country", Bbox: box}}},
		{overture.DivisionsPath(dir, "CH"), 0.005, []cache.Row{{ID: "region", Name: "State", Subtype: "region", Bbox: box}, {ID: "city", Name: "City", Subtype: "locality", Bbox: box}}},
		{overture.AirportsPath(dir), 0, []cache.Row{{ID: "airport", Name: "Airport", Subtype: "airport", Class: "airport", Bbox: box}}},
	} {
		w, err := cache.NewWriter(f.path, cache.Header{Kind: "test", Release: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		point := make([]byte, 21)
		point[0], point[1] = 1, 1
		binary.LittleEndian.PutUint64(point[5:], math.Float64bits(f.lon))
		for _, r := range f.rows {
			if err := w.Add(r, point); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLookupCLIOverrides(t *testing.T) {
	dir := t.TempDir()
	lookupCaches(t, dir)
	profile := `{"countryOverrides":{"CH":{"airports":false,"fallbackDistance":0.002}}}`
	if err := os.WriteFile(filepath.Join(dir, "profiles.json"), []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	a := quiet(dir)
	for _, tc := range []struct {
		flags       []string
		city, state string
		airports    bool
		distance    float64
	}{
		{nil, "Country", "", false, .002},
		{[]string{"-airports=true"}, "Airport", "", true, .002},
		{[]string{"-airports=false", "-fallback-distance", "0.01"}, "City", "State", false, .01},
		{[]string{"-fallback-distance", "0"}, "Country", "", false, 0},
		{nil, "Country", "", false, .002},
	} {
		var out strings.Builder
		a.out = &out
		args := append([]string{"lookup", "-json", "-v"}, tc.flags...)
		args = append(args, "0", "0")
		if err := a.call(args...); err != nil {
			t.Fatal(err)
		}
		var got struct {
			Profile overture.Profile `json:"profile"`
			Final   geocode.Result   `json:"final"`
		}
		if err := json.Unmarshal([]byte(out.String()), &got); err != nil {
			t.Fatalf("JSON %q: %v", out.String(), err)
		}
		if got.Final.City != tc.city || got.Final.State != tc.state || *got.Profile.Airports != tc.airports || *got.Profile.FallbackDistance != tc.distance {
			t.Fatalf("%v: %+v", args, got)
		}
	}
	// With no airports file, a disabled lookup must still finish offline.
	if err := os.Remove(overture.AirportsPath(dir)); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	a.out = &out
	if err := a.call("lookup", "-airports=false", "-fallback-distance", "0", "0", "0"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "fallback=0 airports=false") || !strings.Contains(out.String(), "airports off\n") {
		t.Fatalf("text %q", out.String())
	}
	if _, err := os.Stat(overture.AirportsPath(dir)); !os.IsNotExist(err) {
		t.Fatal("airport cache fetched while disabled")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "profiles.json")); string(got) != profile {
		t.Fatal("catalog changed")
	}
}

func TestDefaultRunSuppressesResolvedLogs(t *testing.T) {
	a := quiet(t.TempDir())
	var logs strings.Builder
	a.errOut = &logs
	if err := a.call("version"); err != nil {
		t.Fatal(err)
	}
	_, err := a.processRun(context.Background(), fixtureStore(2), resolveFunc(namedPoint), runOptions{PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if logs.Len() != 0 {
		t.Fatalf("default log contains per-asset/page diagnostics: %s", logs.String())
	}
}

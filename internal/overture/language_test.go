package overture

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/alexzeitgeist/immich-placenames/internal/cache"
	"github.com/alexzeitgeist/immich-placenames/internal/geo"
	"github.com/alexzeitgeist/immich-placenames/internal/geocode"
)

// writeCaches stores the rows, each over a WKB point at (0, 0).
func writeCaches(t *testing.T, rows map[string][]cache.Row) {
	t.Helper()
	point := make([]byte, 21)
	point[0], point[1] = 1, 1
	for path, rs := range rows {
		w, err := cache.NewWriter(path, cache.Header{Kind: "test", Code: "HR"})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rs {
			if err := w.Add(r, point); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// Each row uses the first available name in the language chain. Caches without
// local names or translations fall back to English and log each shortfall once.
func TestLanguageChain(t *testing.T) {
	dir := t.TempDir()
	box := geo.Bbox{XMin: -1, YMin: -1, XMax: 1, YMax: 1}
	writeCaches(t, map[string][]cache.Row{
		WorldPath(dir): {{ID: "c", Country: "HR", Name: "Croatia", Primary: "Hrvatska", Common: map[string]string{"en": "Croatia", "de": "Kroatien"}, Subtype: "country", Bbox: box}},
		DivisionsPath(dir, "HR"): {
			{ID: "r", Country: "HR", Name: "Dubrovnik-Neretva County", Primary: "Dubrovačko-neretvanska županija",
				Common: map[string]string{"en": "Dubrovnik-Neretva County", "de": "Gespanschaft Dubrovnik-Neretva"}, Subtype: "region", AdminLevel: 1, Bbox: box},
			{ID: "l", Country: "HR", Name: "Old Town", Common: map[string]string{"en": "Old Town"}, Subtype: "locality", AdminLevel: -1, Bbox: box},
		},
		AirportsPath(dir): {{ID: "a", Name: "Dubrovnik Airport", Subtype: "airport", Class: "airport", Bbox: box}},
	})
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		os.WriteFile(path, []byte(body), 0o644)
		return path
	}
	primary := write("primary.json", `{"defaultProfile": {"language": "primary"}}`)
	noAirports := write("no-airports.json", `{"defaultProfile": {"language": "primary"}, "countryOverrides": {"HR": {"airports": false}}}`)
	german := write("de.json", `{"defaultProfile": {"language": "de"}}`)
	var log bytes.Buffer
	for _, c := range []struct {
		path   string
		want   geocode.Result
		served map[string]int
	}{
		{"", geocode.Result{City: "Dubrovnik Airport", State: "Dubrovnik-Neretva County", Country: "Croatia", Found: true}, map[string]int{"en": 6}},
		{primary, geocode.Result{City: "Dubrovnik Airport", State: "Dubrovačko-neretvanska županija", Country: "Hrvatska", Found: true}, map[string]int{"primary": 4, "en": 2}},
		{noAirports, geocode.Result{City: "Old Town", State: "Dubrovačko-neretvanska županija", Country: "Hrvatska", Found: true}, map[string]int{"primary": 4, "en": 2}},
		{german, geocode.Result{City: "Dubrovnik Airport", State: "Gespanschaft Dubrovnik-Neretva", Country: "Kroatien", Found: true}, map[string]int{"de": 4, "en": 2}},
	} {
		profiles, err := LoadProfiles(c.path)
		if err != nil {
			t.Fatal(err)
		}
		p := New(dir, profiles, nil, slog.New(slog.NewTextHandler(&log, nil)))
		for i := 0; i < 2; i++ {
			res, err := p.Resolve(context.Background(), geocode.Point{})
			if err != nil || res != c.want {
				t.Errorf("profiles %q: got %+v, %v; want %+v", c.path, res, err, c.want)
			}
		}
		if !reflect.DeepEqual(p.Served(), c.served) {
			t.Errorf("profiles %q: served %v, want %v", c.path, p.Served(), c.served)
		}
		p.Close()
	}
	if n := strings.Count(log.String(), "no local names"); n != 1 {
		t.Errorf("%d local-name warnings, want one for the airports cache:\n%s", n, log.String())
	}
	if n := strings.Count(log.String(), "no translations"); n != 1 {
		t.Errorf("%d translation warnings, want one for the airports cache:\n%s", n, log.String())
	}
}

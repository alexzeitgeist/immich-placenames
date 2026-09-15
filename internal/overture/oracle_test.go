package overture

import (
	"context"
	"encoding/csv"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/alexzeitgeist/immich-placenames/internal/cache"
	"github.com/alexzeitgeist/immich-placenames/internal/geocode"
)

const oracleRelease = "2026-08-19.0"
const oracleProfilePath = "../../testdata/profiles.json"

type sample struct {
	id                   string
	pt                   geocode.Point
	city, state, country string
}

func loadSamples(t *testing.T, path string) []sample {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	reader := csv.NewReader(f)
	reader.FieldsPerRecord = 6
	recs, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) < 2 || (recs[0][0] != "caseId" && recs[0][0] != "assetId") || strings.Join(recs[0][1:], ",") != "lat,lon,city,state,country" {
		t.Fatalf("%s: expected caseId,lat,lon,city,state,country and at least one sample", path)
	}
	var out []sample
	seen := map[string]bool{}
	for _, r := range recs[1:] {
		lat, latErr := strconv.ParseFloat(r[1], 64)
		lon, lonErr := strconv.ParseFloat(r[2], 64)
		if r[0] == "" || seen[r[0]] || latErr != nil || lonErr != nil || math.IsNaN(lat) || math.IsNaN(lon) || math.Abs(lat) > 90 || math.Abs(lon) > 180 {
			t.Fatalf("%s: invalid or duplicate sample %q", path, r[0])
		}
		seen[r[0]] = true
		out = append(out, sample{r[0], geocode.Point{Lat: lat, Lon: lon}, r[3], r[4], r[5]})
	}
	return out
}

var oracleCodes = map[string]string{
	"Austria": "AT", "Croatia": "HR", "Germany": "DE", "India": "IN",
	"Indonesia": "ID", "Switzerland": "CH", "United States": "US",
}

func oracleCachePaths(dir string) []string {
	paths := []string{WorldPath(dir), AirportsPath(dir)}
	for _, code := range []string{"AT", "CH", "DE", "HR", "ID", "IN", "US"} {
		paths = append(paths, DivisionsPath(dir, code))
	}
	return paths
}

// Opt-in tests require complete, pinned caches. Cache-directory profiles are
// never read. A nil Fetcher prevents all on-demand downloads.
func oracleDataDir(t testing.TB) string {
	t.Helper()
	dir := oracleRoot(t)
	checkRelease(t, oracleCachePaths(dir)...)
	return dir
}

// oracleRoot skips the test unless REVERSEGEO_DATA is set.
func oracleRoot(t testing.TB) string {
	t.Helper()
	dir := os.Getenv("REVERSEGEO_DATA")
	if dir == "" {
		t.Skip("REVERSEGEO_DATA not set")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func checkRelease(t testing.TB, paths ...string) {
	t.Helper()
	for _, path := range paths {
		f, err := cache.Open(path)
		if err != nil {
			t.Fatalf("oracle cache %s: %v", path, err)
		}
		release := f.Header.Release
		f.Close()
		if release != oracleRelease {
			t.Fatalf("%s: release %q, want %s", path, release, oracleRelease)
		}
	}
}

func openData(t testing.TB) *Provider {
	t.Helper()
	dir := oracleDataDir(t)
	profiles, err := LoadProfiles(oracleProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	return New(dir, profiles, nil, slog.Default())
}

func checkNames(t *testing.T, p *Provider, samples []sample) {
	t.Helper()
	for _, s := range samples {
		t.Run(s.id, func(t *testing.T) {
			res, err := p.Resolve(context.Background(), s.pt)
			if err != nil {
				t.Fatal(err)
			}
			got := res
			if !got.Found || got.City != s.city || got.State != s.state || got.Country != s.country {
				t.Errorf("got %+v; want %q, %q, %q", got, s.city, s.state, s.country)
			}
			// Bounded and exact distances must give the same result.
			e, err := p.Explain(context.Background(), s.pt)
			if err != nil {
				t.Fatal(err)
			}
			if e.Result != res {
				t.Errorf("explain %+v; resolve %+v", e.Result, res)
			}
		})
	}
}

// Named reference points are an Overture regression suite, not an expectation
// of other providers or a claim of independent geographic ground truth.
func TestOracle(t *testing.T) {
	p := openData(t)
	defer p.Close()
	samples := loadSamples(t, "../../testdata/samples.csv")
	ctx := context.Background()
	t.Run("country", func(t *testing.T) {
		for _, s := range samples {
			code, _, found, err := p.Country(ctx, s.pt)
			if err != nil {
				t.Fatal(err)
			}
			if want := oracleCodes[s.country]; want == "" || !found || code != want {
				t.Errorf("%s: country %q found=%v, want %s", s.id, code, found, want)
			}
		}
	})
	t.Run("world only", func(t *testing.T) {
		wd := t.TempDir()
		if err := os.Symlink(WorldPath(p.Dir), WorldPath(wd)); err != nil {
			t.Fatal(err)
		}
		q := New(wd, p.Profiles, nil, slog.Default())
		defer q.Close()
		for _, s := range samples {
			a, _, af, err1 := p.Country(ctx, s.pt)
			b, _, bf, err2 := q.Country(ctx, s.pt)
			if err1 != nil || err2 != nil || a != b || af != bf {
				t.Errorf("%s: full %q/%v, world only %q/%v (%v %v)", s.id, a, af, b, bf, err1, err2)
			}
		}
	})
	t.Run("names", func(t *testing.T) { checkNames(t, p, samples) })
}

func TestSeaFallback(t *testing.T) {
	p := openData(t)
	defer p.Close()
	samples := loadSamples(t, "../../testdata/sea.csv")
	checkNames(t, p, samples)
	for _, s := range samples {
		t.Run(s.id+"/bounded fallback", func(t *testing.T) {
			e, err := p.Explain(context.Background(), s.pt)
			if err != nil {
				t.Fatal(err)
			}
			nearest := false
			for _, d := range e.Divisions {
				if d.Name == s.city && strings.Contains(d.Decision, "city") && !d.Contains && d.Distance > 0 && d.Distance <= *e.Profile.FallbackDistance {
					nearest = true
				}
			}
			if !nearest {
				t.Fatal("fixture no longer exercises a nearest-division city")
			}
			zero := 0.0
			p.Overrides.FallbackDistance = &zero
			defer func() { p.Overrides.FallbackDistance = nil }()
			res, err := p.Resolve(context.Background(), s.pt)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Found || res.City != s.state {
				t.Fatalf("nearest division disabled: got %+v; want the state filling the city", res)
			}
		})
	}
}

func TestOracleAirportOverride(t *testing.T) {
	p := openData(t)
	defer p.Close()
	for _, c := range []struct {
		id            string
		pt            geocode.Point
		airport, city string
	}{
		{"zurich", geocode.Point{Lat: 47.46, Lon: 8.55}, "Zürich Airport", "Kloten"},
		{"dubrovnik", geocode.Point{Lat: 42.56, Lon: 18.27}, "Dubrovnik Ruđer Bošković Airport", "Konavle"},
	} {
		t.Run(c.id, func(t *testing.T) {
			for _, enabled := range []bool{true, false} {
				p.Overrides.Airports = &enabled
				res, err := p.Resolve(context.Background(), c.pt)
				if err != nil {
					t.Fatal(err)
				}
				want := c.city
				if enabled {
					want = c.airport
				}
				if res.City != want {
					t.Errorf("airports=%v: city %q, want %q", enabled, res.City, want)
				}
			}
		})
	}
}

func TestOracleIgnoresCacheProfile(t *testing.T) {
	dir := oracleDataDir(t)
	isolated := t.TempDir()
	if err := os.Mkdir(filepath.Join(isolated, "divisions"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, path := range oracleCachePaths(dir) {
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(isolated, rel)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(isolated, "profiles.json"), []byte("invalid profile"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVERSEGEO_DATA", isolated)
	p := openData(t)
	defer p.Close()
	checkNames(t, p, loadSamples(t, "../../testdata/samples.csv"))
}

// TestPrivateOracle checks optional user-supplied CSVs with the pinned profile.
func TestPrivateOracle(t *testing.T) {
	dir := os.Getenv("REVERSEGEO_PRIVATE_FIXTURES")
	if dir == "" {
		t.Skip("REVERSEGEO_PRIVATE_FIXTURES not set")
	}
	p := openData(t)
	defer p.Close()
	for _, name := range []string{"samples.csv", "sea.csv"} {
		t.Run(name, func(t *testing.T) { checkNames(t, p, loadSamples(t, filepath.Join(dir, name))) })
	}
}

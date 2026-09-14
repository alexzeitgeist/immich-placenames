package overture

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/alexzeitgeist/immich-placenames/internal/cache"
	"github.com/alexzeitgeist/immich-placenames/internal/geocode"
)

// TestOracleLocalNames checks the Lapad grid point in Croatian and German.
func TestOracleLocalNames(t *testing.T) {
	dir := oracleDataDir(t)
	hr, err := cache.Open(DivisionsPath(dir, "HR"))
	if err != nil {
		t.Fatal(err)
	}
	hasPrimary, hasCommon := hr.HasPrimary, hr.HasCommon
	hr.Close()
	var base Catalog
	b, err := os.ReadFile(oracleProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &base); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		language string
		have     bool
		want     geocode.Result
	}{
		{LanguagePrimary, hasPrimary, geocode.Result{City: "Dubrovnik", State: "Dubrovačko-neretvanska županija", Country: "Hrvatska", Found: true}},
		{"de", hasCommon, geocode.Result{City: "Dubrovnik", State: "Gespanschaft Dubrovnik-Neretva", Country: "Kroatien", Found: true}},
	} {
		if !c.have {
			t.Fatalf("HR cache cannot serve %s; refetch the pinned release", c.language)
		}
		cat := base
		cat.DefaultProfile.Language = Languages{c.language}
		b, err := json.Marshal(cat)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "profiles.json")
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		profiles, err := LoadProfiles(path)
		if err != nil {
			t.Fatal(err)
		}
		p := New(dir, profiles, nil, slog.Default())
		res, err := p.Resolve(context.Background(), geocode.Point{Lat: 42.65, Lon: 18.07})
		p.Close()
		if err != nil || res != c.want {
			t.Errorf("%s: got %+v, %v; want %+v", c.language, res, err, c.want)
		}
	}
}

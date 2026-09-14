package overture

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexzeitgeist/immich-placenames/internal/cache"
	"github.com/alexzeitgeist/immich-placenames/internal/geo"
	"github.com/alexzeitgeist/immich-placenames/internal/geocode"
)

// A valid index can still reference a damaged or unreadable geometry blob.
// Neither failure may turn into a writable state/country fallback.
func TestGeometryFailure(t *testing.T) {
	for _, kind := range []string{"world", "divisions", "airports"} {
		for _, failure := range []string{"decode", "read"} {
			t.Run(kind+"/"+failure, func(t *testing.T) {
				dir := t.TempDir()
				profiles, err := LoadProfiles("")
				if err != nil {
					t.Fatal(err)
				}
				p := New(dir, profiles, nil, nil)
				defer p.Close()
				for _, k := range []string{"world", "divisions", "airports"} {
					path := WorldPath(dir)
					row := cache.Row{ID: "country", Country: "CH", Name: "Switzerland", Subtype: "country", Bbox: geo.Bbox{XMin: -1, YMin: -1, XMax: 1, YMax: 1}}
					switch k {
					case "divisions":
						path = DivisionsPath(dir, "CH")
						row.ID = "city"
						row.Name = "Real City"
						row.Subtype = "locality"
					case "airports":
						path = AirportsPath(dir)
						row.ID = "airport"
						row.Name = "Real Airport"
						row.Subtype = "airport"
						row.Class = "airport"
					}
					// Little-endian WKB point at (0, 0).
					blob := make([]byte, 21)
					blob[0] = 1
					blob[1] = 1
					if k == kind && failure == "decode" {
						blob = []byte{255}
					}
					w, err := cache.NewWriter(path, cache.Header{Kind: k, Code: "CH"})
					if err != nil {
						t.Fatal(err)
					}
					if err = w.Add(row, blob); err != nil {
						w.Abort()
						t.Fatal(err)
					}
					if err = w.Close(); err != nil {
						t.Fatal(err)
					}
					if k == kind && failure == "read" {
						f, err := cache.Open(path)
						if err != nil {
							t.Fatal(err)
						}
						switch k {
						case "world":
							p.world = f
						case "divisions":
							p.div["CH"] = f
						case "airports":
							p.air = f
						}
						if err = f.Close(); err != nil {
							t.Fatal(err)
						}
					}
				}
				ctx := context.Background()
				pt := geocode.Point{}
				result, err := p.Resolve(ctx, pt)
				if err == nil || result != (geocode.Result{}) {
					t.Fatalf("Resolve: result=%+v err=%v", result, err)
				}
				if !strings.Contains(err.Error(), filepath.Base(map[string]string{"world": WorldPath(dir), "divisions": DivisionsPath(dir, "CH"), "airports": AirportsPath(dir)}[kind])) {
					t.Errorf("error lacks cache path: %v", err)
				}
				if e, err := p.Explain(ctx, pt); err == nil || e != nil {
					t.Fatalf("Explain: result=%+v err=%v", e, err)
				}
				outcomes, err := geocode.ResolveAll(ctx, p, []geocode.Asset{{ID: "asset", Point: pt}})
				if err != nil || len(outcomes) != 1 || outcomes[0].Err == nil || outcomes[0].Result.Writable() {
					t.Fatalf("ResolveAll: outcomes=%+v err=%v", outcomes, err)
				}
				if kind == "world" {
					code, name, found, err := p.Country(ctx, pt)
					if err == nil || found || code != "" || name != "" {
						t.Fatalf("Country: %q %q found=%v err=%v", code, name, found, err)
					}
				}
			})
		}
	}
}

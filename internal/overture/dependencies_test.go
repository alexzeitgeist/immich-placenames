package overture

import (
	"context"
	"log/slog"
	"math"
	"slices"
	"testing"

	"github.com/alexzeitgeist/immich-placenames/internal/geo"
	"github.com/alexzeitgeist/immich-placenames/internal/geocode"
)

// dependencyCodes covers 105 polygons in release 2026-08-19.0.
var dependencyCodes = []string{
	"AI", "AS", "AW", "AX", "BL", "BM", "BQ", "BV", "CC", "CK", "CP", "CW", "CX",
	"FK", "FO", "GF", "GG", "GI", "GL", "GP", "GS", "GU", "HK", "HM", "IM", "IO",
	"JE", "KY", "MO", "MP", "MQ", "MS", "NC", "NF", "NU", "PF", "PM", "PN", "PR",
	"RE", "SH", "SJ", "TC", "TF", "TK", "UM", "VG", "VI", "WF", "XE", "XJ", "XS", "YT",
}

// insidePoint starts at the bbox centre and walks towards the nearest edge.
// Edge points within geo.Tolerance count as contained, as in the resolver.
func insidePoint(g *geo.Geometry) (geocode.Point, bool) {
	const bearings = 64
	x, y := g.Bbox().Center()
	for round := 0; round < 24; round++ {
		if in, _ := g.Locate(x, y, geo.Tolerance); in {
			return geocode.Point{Lat: y, Lon: x}, true
		}
		d := g.Distance(x, y)
		if math.IsInf(d, 1) {
			break
		}
		bx, by, best := x, y, d
		for k := 0; k < bearings; k++ {
			a := 2 * math.Pi * float64(k) / bearings
			nx, ny := x+d*math.Cos(a), y+d*math.Sin(a)
			if nd := g.Distance(nx, ny); nd < best {
				bx, by, best = nx, ny, nd
			}
		}
		if best >= d {
			break
		}
		x, y = bx, by
	}
	return geocode.Point{Lat: y, Lon: x}, false
}

func TestOracleDependencies(t *testing.T) {
	dir := oracleRoot(t)
	checkRelease(t, WorldPath(dir))
	profiles, err := LoadProfiles(oracleProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	p := New(dir, profiles, nil, slog.Default())
	defer p.Close()
	ctx := context.Background()
	w, err := p.World(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !w.Header.Dependencies {
		t.Fatalf("%s holds no dependency territories; refetch it", w.Path)
	}
	seen := map[string]bool{}
	rows := 0
	for i, r := range w.Rows {
		if r.Subtype != "dependency" {
			continue
		}
		rows++
		seen[r.Country] = true
		g, err := w.Geometry(i)
		if err != nil {
			t.Fatal(err)
		}
		pt, ok := insidePoint(g)
		if !ok {
			t.Errorf("%s %s: no point found inside the polygon", r.Country, r.ID)
			continue
		}
		code, name, found, err := p.Country(ctx, pt)
		if err != nil {
			t.Fatal(err)
		}
		if !found || code != r.Country || name != r.Name {
			t.Errorf("%s %s at %.5f,%.5f: got %q %q found=%v", r.Country, r.ID, pt.Lat, pt.Lon, code, name, found)
		}
	}
	if rows != 105 {
		t.Errorf("got %d dependency polygons, want 105", rows)
	}
	codes := make([]string, 0, len(seen))
	for c := range seen {
		codes = append(codes, c)
	}
	slices.Sort(codes)
	if !slices.Equal(codes, dependencyCodes) {
		t.Errorf("%d dependency polygons over codes %v, want %v", rows, codes, dependencyCodes)
	}
}

// Fixed coordinates complement the generated points above.
func TestOracleDependencyPlaces(t *testing.T) {
	dir := oracleRoot(t)
	checkRelease(t, WorldPath(dir))
	profiles, err := LoadProfiles(oracleProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	p := New(dir, profiles, nil, slog.Default())
	defer p.Close()
	for _, c := range []struct {
		id            string
		pt            geocode.Point
		code, country string
	}{
		{"hong kong", geocode.Point{Lat: 22.2800, Lon: 114.1600}, "HK", "Hong Kong"},
		{"macao", geocode.Point{Lat: 22.1987, Lon: 113.5439}, "MO", "Macau"},
		{"nuuk", geocode.Point{Lat: 64.1750, Lon: -51.7380}, "GL", "Greenland"},
		{"saint-denis", geocode.Point{Lat: -20.8820, Lon: 55.4500}, "RE", "Réunion"},
		{"douglas", geocode.Point{Lat: 54.1500, Lon: -4.4800}, "IM", "Isle of Man"},
		{"san juan", geocode.Point{Lat: 18.4655, Lon: -66.1057}, "PR", "Puerto Rico"},
		{"tórshavn", geocode.Point{Lat: 62.0100, Lon: -6.7700}, "FO", "Faroe Islands"},
		{"mariehamn", geocode.Point{Lat: 60.0970, Lon: 19.9350}, "AX", "Åland Islands"},
		{"gibraltar", geocode.Point{Lat: 36.1408, Lon: -5.3536}, "GI", "Gibraltar"},
		{"hamilton", geocode.Point{Lat: 32.2949, Lon: -64.7830}, "BM", "Bermuda"},
		{"papeete", geocode.Point{Lat: -17.5350, Lon: -149.5695}, "PF", "French Polynesia"},
		{"nouméa", geocode.Point{Lat: -22.2758, Lon: 166.4580}, "NC", "New Caledonia"},
		{"oranjestad", geocode.Point{Lat: 12.5240, Lon: -70.0270}, "AW", "Aruba"},
	} {
		t.Run(c.id, func(t *testing.T) {
			code, name, found, err := p.Country(context.Background(), c.pt)
			if err != nil {
				t.Fatal(err)
			}
			if !found || code != c.code || name != c.country {
				t.Errorf("got %q %q found=%v; want %s %q", code, name, found, c.code, c.country)
			}
		})
	}
}

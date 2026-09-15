package overture

import (
	"context"
	"testing"

	"github.com/alexzeitgeist/immich-placenames/internal/geocode"
)

// These benchmarks require REVERSEGEO_DATA; see testdata/README.md.

// Berlin has candidates that cross the antimeridian. The Atlantic point
// matches no country.
func BenchmarkCountry(b *testing.B) {
	dir := oracleRoot(b)
	checkRelease(b, WorldPath(dir))
	profiles, err := LoadProfiles(oracleProfilePath)
	if err != nil {
		b.Fatal(err)
	}
	p := New(dir, profiles, nil, nil)
	defer p.Close()
	ctx := context.Background()
	for _, c := range []struct {
		name string
		pt   geocode.Point
	}{
		{"berlin", geocode.Point{Lat: 52.52, Lon: 13.405}},
		{"tokyo", geocode.Point{Lat: 35.68, Lon: 139.77}},
		{"atlantic", geocode.Point{Lat: 30, Lon: -40}},
	} {
		b.Run(c.name, func(b *testing.B) {
			// Decode once before timing repeated lookups.
			if _, _, _, err := p.Country(ctx, c.pt); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				if _, _, _, err := p.Country(ctx, c.pt); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkResolve(b *testing.B) {
	p := openData(b)
	defer p.Close()
	ctx := context.Background()
	pt := geocode.Point{Lat: 42.641, Lon: 18.109}
	for _, airports := range []bool{false, true} {
		name := "divisions"
		if airports {
			name = "divisions and airports"
		}
		b.Run(name, func(b *testing.B) {
			p.Overrides.Airports = &airports
			defer func() { p.Overrides.Airports = nil }()
			if _, err := p.Resolve(ctx, pt); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := p.Resolve(ctx, pt); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

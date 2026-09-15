package geocode

import (
	"context"
	"errors"
	"math"
	"testing"
)

type fake func(Point) (Result, error)

func TestPointValid(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	for _, p := range []Point{{90, 180}, {-90, -180}, {0, 0}} {
		if !p.Valid() {
			t.Errorf("%v invalid", p)
		}
	}
	for _, p := range []Point{{90.1, 0}, {0, -180.1}, {nan, 0}, {0, nan}, {inf, 0}, {0, -inf}} {
		if p.Valid() {
			t.Errorf("%v valid", p)
		}
	}
}

func (f fake) Resolve(_ context.Context, p Point) (Result, error) { return f(p) }

func resolver(p Point) (Result, error) {
	switch p.Lat {
	case 1:
		return Result{State: "State", Country: "Country", Found: true}, nil
	case 2:
		return Result{}, nil
	case 3:
		return Result{City: "leaked", Country: "leaked", Found: true}, errors.New("boom")
	case 4:
		return Result{City: "orphan", Found: true}, nil
	}
	return Result{City: "City", State: "State", Country: "Country", Found: true}, nil
}

func TestResolveAll(t *testing.T) {
	assets := []Asset{{"a", Point{1, 0}}, {"b", Point{2, 0}}, {"c", Point{3, 0}}, {"d", Point{4, 0}}, {"e", Point{5, 0}}}
	out, err := ResolveAll(context.Background(), fake(resolver), assets)
	if err != nil || len(out) != 5 {
		t.Fatalf("outcomes %d err %v", len(out), err)
	}
	if o := out[0]; o.Err != nil || o.Result.City != "" || !o.Result.Writable() {
		t.Errorf("resolver answer altered: the city fallback is the resolver's, got %+v", o)
	}
	if o := out[1]; o.Err != nil || o.Result.Found || o.Result.City != "" || o.Result.Writable() {
		t.Errorf("no match must stay empty, got %+v", o)
	}
	if o := out[2]; o.Err == nil || o.Result != (Result{}) {
		t.Errorf("error must carry no fields, got %+v", o)
	}
	if o := out[3]; !errors.Is(o.Err, ErrNoCountry) || o.Result != (Result{}) {
		t.Errorf("place without country must be an error, got %+v", o)
	}
	if o := out[4]; o.Err != nil || o.Result.City != "City" {
		t.Errorf("full result altered, got %+v", o)
	}
}

func TestResolveAllCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := ResolveAll(ctx, fake(resolver), []Asset{{"a", Point{1, 0}}})
	if !errors.Is(err, context.Canceled) || len(out) != 0 {
		t.Errorf("want cancellation with no outcomes, got %d %v", len(out), err)
	}
}

func TestFillCity(t *testing.T) {
	both := []string{SourceState, SourceCountry}
	county := map[string]string{"county": "County"}
	for _, tc := range []struct {
		name       string
		in         Result
		sources    []string
		extra      map[string]string
		city, from string
	}{
		{"state before country", Result{State: "S", Country: "C", Found: true}, both, nil, "S", SourceState},
		{"country when the state is empty", Result{Country: "C", Found: true}, both, nil, "C", SourceCountry},
		{"country alone", Result{State: "S", Country: "C", Found: true}, []string{SourceCountry}, nil, "C", SourceCountry},
		{"listed order", Result{State: "S", Country: "C", Found: true}, []string{SourceCountry, SourceState}, nil, "C", SourceCountry},
		{"no sources", Result{State: "S", Country: "C", Found: true}, nil, nil, "", ""},
		{"city kept", Result{City: "City", State: "S", Country: "C", Found: true}, both, county, "City", ""},
		{"nothing to fill from", Result{Found: true}, both, nil, "", ""},
		{"not found", Result{State: "S", Country: "C"}, both, county, "", ""},
		{"a source of the resolver's own", Result{State: "S", Country: "C", Found: true}, []string{"county", SourceState}, county, "County", "county"},
		{"an unsupplied source is skipped", Result{State: "S", Country: "C", Found: true}, []string{"borough", SourceState}, county, "S", SourceState},
		{"state and country are never taken from extra", Result{Found: true}, both, map[string]string{SourceState: "X"}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, from := tc.in.FillCity(tc.sources, tc.extra)
			if got.City != tc.city || from != tc.from {
				t.Errorf("city %q from %q; want %q from %q", got.City, from, tc.city, tc.from)
			}
			if got.State != tc.in.State || got.Country != tc.in.Country || got.Found != tc.in.Found {
				t.Errorf("state, country or match changed: %+v", got)
			}
		})
	}
}

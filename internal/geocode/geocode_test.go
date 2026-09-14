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
	if o := out[0]; o.Err != nil || o.Result.City != "State" || !o.Result.Writable() {
		t.Errorf("resolved without city: fallback expected, got %+v", o)
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

func TestWithFallback(t *testing.T) {
	if r := (Result{Country: "C", Found: true}).WithFallback(); r.City != "C" {
		t.Errorf("country fallback, got %+v", r)
	}
	if r := (Result{State: "S", Country: "C", Found: true}).WithFallback(); r.City != "S" {
		t.Errorf("state fallback, got %+v", r)
	}
	if r := (Result{State: "S", Country: "C"}).WithFallback(); r.City != "" {
		t.Errorf("not found must not receive a fallback, got %+v", r)
	}
}

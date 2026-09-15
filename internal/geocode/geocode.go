// Package geocode defines the resolver interface, results and city fallback.
package geocode

import (
	"context"
	"errors"
)

// Point is a WGS84 coordinate.
type Point struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// Valid reports whether the coordinate is finite and within ±90 and ±180;
// NaN fails every comparison.
func (p Point) Valid() bool {
	return p.Lat >= -90 && p.Lat <= 90 && p.Lon >= -180 && p.Lon <= 180
}

// Result names the place at a point. Found false is a valid answer, not a failure.
type Result struct {
	City    string `json:"city"`
	State   string `json:"state"`
	Country string `json:"country"`
	Found   bool   `json:"found"`
}

// Resolver names a point. A nil error with Found false means no match.
// Operational failures return an error.
type Resolver interface {
	Resolve(ctx context.Context, p Point) (Result, error)
}

// Fallback sources available from Result.
const (
	SourceState   = "state"
	SourceCountry = "country"
)

// FillCity fills an empty city from the first nonempty source and returns its key.
// State and country come from r; other sources come from extra.
// Unmatched results and existing city names are unchanged.
func (r Result) FillCity(sources []string, extra map[string]string) (Result, string) {
	if !r.Found || r.City != "" {
		return r, ""
	}
	for _, s := range sources {
		name := extra[s]
		switch s {
		case SourceState:
			name = r.State
		case SourceCountry:
			name = r.Country
		}
		if name != "" {
			r.City = name
			return r, s
		}
	}
	return r, ""
}

// Writable reports whether the result may be written to Immich: found, with a country.
func (r Result) Writable() bool { return r.Found && r.Country != "" }

// Asset is one Immich asset to resolve.
type Asset struct {
	ID    string
	Point Point
}

// Outcome is the result of resolving one asset: resolved, no match, or error.
// When Err is set, Result is the zero value.
type Outcome struct {
	Asset  Asset
	Result Result
	Err    error
}

// ErrNoCountry marks a resolver answer that claims a place without a country.
var ErrNoCountry = errors.New("resolver returned a place without a country")

// ResolveAll resolves assets in order. The resolver handles city fallback.
// Cancellation returns partial outcomes and the context error.
// Per-asset errors are recorded in the outcomes; processing continues.
func ResolveAll(ctx context.Context, r Resolver, assets []Asset) ([]Outcome, error) {
	out := make([]Outcome, 0, len(assets))
	for _, a := range assets {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		res, err := r.Resolve(ctx, a.Point)
		switch {
		case err != nil:
			out = append(out, Outcome{Asset: a, Err: err})
		case res.Found && res.Country == "":
			out = append(out, Outcome{Asset: a, Err: ErrNoCountry})
		default:
			out = append(out, Outcome{Asset: a, Result: res})
		}
	}
	return out, nil
}

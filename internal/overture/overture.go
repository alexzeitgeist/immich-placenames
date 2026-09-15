// Package overture resolves country, state and city from cached Overture
// polygons, with optional airport names in place of the city.
package overture

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alexzeitgeist/immich-placenames/internal/cache"
	"github.com/alexzeitgeist/immich-placenames/internal/geo"
	"github.com/alexzeitgeist/immich-placenames/internal/geocode"
)

var airportRank = map[string]int{
	"international_airport": 0,
	"regional_airport":      1,
	"municipal_airport":     2,
	"airport":               3,
	"private_airport":       4,
	"airfield":              5,
	"seaplane_airport":      6,
	"heliport":              7,
}

// Fetcher builds a missing cache file on demand. Nil disables fetching.
type Fetcher interface {
	// World uses the fetcher's default release when release is empty.
	World(ctx context.Context, release, dest string) error
	Divisions(ctx context.Context, code string, box geo.Bbox, dest string) error
	Airports(ctx context.Context, dest string) error
}

// Cache file locations under the data directory.
func WorldPath(dir string) string           { return filepath.Join(dir, "world.geo") }
func AirportsPath(dir string) string        { return filepath.Join(dir, "airports.geo") }
func DivisionsPath(dir, code string) string { return filepath.Join(dir, "divisions", code+".geo") }

// Provider resolves points; not safe for concurrent use.
type Provider struct {
	Dir      string
	Profiles *Profiles
	Fetch    Fetcher
	Log      *slog.Logger
	// Overrides is an optional in-memory profile applied after the catalog.
	// Its zero-valued fields inherit the effective catalog profile.
	Overrides Profile

	world    *cache.File
	worldErr error
	div      map[string]*cache.File
	divErr   map[string]error
	air      *cache.File
	airErr   error
	reported map[string]bool // cache shortfalls already logged
	served   map[string]int  // names written, by the language that served them
}

// New returns a provider over dir; profiles may be nil when it only fetches.
func New(dir string, profiles *Profiles, fetch Fetcher, log *slog.Logger) *Provider {
	if log == nil {
		log = slog.Default()
	}
	return &Provider{Dir: dir, Profiles: profiles, Fetch: fetch, Log: log,
		div: map[string]*cache.File{}, divErr: map[string]error{}, reported: map[string]bool{}, served: map[string]int{}}
}

// open logs invalid cache files and treats them as absent.
func (p *Provider) open(path string) (*cache.File, error) {
	f, err := cache.Open(path)
	if errors.Is(err, cache.ErrInvalid) {
		p.Log.Warn("cache invalid, treated as absent", "err", err)
		return nil, fs.ErrNotExist
	}
	return f, err
}

// World opens the world cache, fetching it if missing or lacking dependencies.
func (p *Provider) World(ctx context.Context) (*cache.File, error) {
	if p.world != nil {
		return p.world, nil
	}
	if p.worldErr != nil {
		return nil, p.worldErr
	}
	path := WorldPath(p.Dir)
	f, err := p.open(path)
	if errors.Is(err, fs.ErrNotExist) && p.Fetch != nil {
		p.Log.Info("fetching country polygons", "path", path)
		if err = p.Fetch.World(ctx, "", path); err == nil {
			f, err = p.open(path)
		}
	}
	if err == nil && !f.Header.Dependencies {
		f, err = p.withDependencies(ctx, f)
	}
	if err != nil {
		p.worldErr = fmt.Errorf("world cache: %w", err)
		return nil, p.worldErr
	}
	p.world = f
	return f, nil
}

// withDependencies preserves the release and keeps the old cache on fetch failure.
func (p *Provider) withDependencies(ctx context.Context, old *cache.File) (*cache.File, error) {
	if p.Fetch == nil {
		p.Log.Warn("cache has no dependency territories, Hong Kong and the like go unnamed; refetch it", "path", old.Path)
		return old, nil
	}
	p.Log.Info("refetching country polygons for the dependency territories", "path", old.Path, "release", old.Header.Release)
	if err := p.Fetch.World(ctx, old.Header.Release, old.Path); err != nil {
		p.Log.Warn("refetch failed, keeping the country polygons without dependency territories", "path", old.Path, "err", err)
		return old, nil
	}
	f, err := p.open(old.Path)
	old.Close()
	return f, err
}

// Divisions opens a country's divisions, fetching them when absent. A failure
// is remembered for the process.
func (p *Provider) Divisions(ctx context.Context, code string) (*cache.File, error) {
	if f := p.div[code]; f != nil {
		return f, nil
	}
	if err := p.divErr[code]; err != nil {
		return nil, err
	}
	path := DivisionsPath(p.Dir, code)
	f, err := p.open(path)
	if errors.Is(err, fs.ErrNotExist) && p.Fetch != nil {
		var box geo.Bbox
		box, err = p.CountryBbox(ctx, code)
		if err == nil {
			p.Log.Info("fetching divisions", "country", code, "path", path)
			if err = p.Fetch.Divisions(ctx, code, box, path); err == nil {
				f, err = p.open(path)
			}
		}
	}
	if err != nil {
		p.divErr[code] = fmt.Errorf("divisions %s: %w", code, err)
		return nil, p.divErr[code]
	}
	p.div[code] = f
	return f, nil
}

// Airports opens the airports, fetching them when absent. A failure is
// remembered for the process.
func (p *Provider) Airports(ctx context.Context) (*cache.File, error) {
	if p.air != nil {
		return p.air, nil
	}
	if p.airErr != nil {
		return nil, p.airErr
	}
	path := AirportsPath(p.Dir)
	f, err := p.open(path)
	if errors.Is(err, fs.ErrNotExist) && p.Fetch != nil {
		p.Log.Info("fetching airports", "path", path)
		if err = p.Fetch.Airports(ctx, path); err == nil {
			f, err = p.open(path)
		}
	}
	if err != nil {
		p.airErr = fmt.Errorf("airports cache: %w", err)
		return nil, p.airErr
	}
	p.air = f
	return f, nil
}

// CountryBbox unions the bboxes of the country's polygons in world.geo.
func (p *Provider) CountryBbox(ctx context.Context, code string) (geo.Bbox, error) {
	w, err := p.World(ctx)
	if err != nil {
		return geo.Bbox{}, err
	}
	var box geo.Bbox
	found := false
	for _, r := range w.Rows {
		if r.Country != code {
			continue
		}
		if found {
			box = box.Union(r.Bbox)
		} else {
			box, found = r.Bbox, true
		}
	}
	if !found {
		return box, fmt.Errorf("country %s not in %s", code, w.Path)
	}
	return box, nil
}

// Releases lists the opened caches and their releases.
func (p *Provider) Releases() map[string]string {
	out := map[string]string{}
	if p.world != nil {
		out["world"] = p.world.Header.Release
	}
	for c, f := range p.div {
		out["divisions/"+c] = f.Header.Release
	}
	if p.air != nil {
		out["airports"] = p.air.Header.Release
	}
	return out
}

// Close releases the cache files.
func (p *Provider) Close() {
	if p.world != nil {
		p.world.Close()
	}
	for _, f := range p.div {
		f.Close()
	}
	if p.air != nil {
		p.air.Close()
	}
}

// Candidate is a cache row whose bbox contains the point. Name is the first
// available name in the profile's language order; Language records its language.
type Candidate struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Language    string   `json:"language"`
	Subtype     string   `json:"subtype"`
	Class       string   `json:"class"`
	Country     string   `json:"country"`
	AdminLevel  int32    `json:"adminLevel"`
	Territorial bool     `json:"territorial"`
	Bbox        geo.Bbox `json:"bbox"`
	Contains    bool     `json:"contains"`
	Distance    float64  `json:"distance"` // planar degrees to the geometry, 0 when contained
	Decision    string   `json:"decision"`
	row         int
}

// Area is the bounding-box area used to break ranking ties.
func (c Candidate) Area() float64 { return c.Bbox.Area() }

// chain trims a profile's languages to what a cache can serve: primary needs
// local names, translation codes need the translations. A dropped code that
// the profile ranks above en is reported once per cache; en always remains.
func (p *Provider) chain(f *cache.File, want Languages) Languages {
	out := make(Languages, 0, len(want))
	preferred := true
	for _, code := range want {
		switch {
		case code == LanguageEnglish:
			preferred = false
			out = append(out, code)
		case code == LanguagePrimary && !f.HasPrimary:
			if preferred {
				p.report(f, "local names")
			}
		case code != LanguagePrimary && !f.HasCommon:
			if preferred {
				p.report(f, "translations")
			}
		default:
			out = append(out, code)
		}
	}
	return out
}

func (p *Provider) report(f *cache.File, what string) {
	key := f.Path + " " + what
	if !p.reported[key] {
		p.reported[key] = true
		p.Log.Warn("cache has no "+what+", falling back; refetch it", "path", f.Path)
	}
}

// nameOf names row i by the first language in chain that has a name, then the
// id; it returns the language used. In a cache without translations, Name,
// English where Overture had it, stands in for en.
func nameOf(f *cache.File, i int, chain Languages) (string, string) {
	r := f.Rows[i]
	for _, code := range chain {
		switch code {
		case LanguagePrimary:
			if r.Primary != "" {
				return r.Primary, code
			}
		case LanguageEnglish:
			if v := r.Common[code]; v != "" {
				return v, code
			}
			if !f.HasCommon && r.Name != "" {
				return r.Name, code
			}
		default:
			if v := r.Common[code]; v != "" {
				return v, code
			}
		}
	}
	return r.Name, "id"
}

// Served counts the names resolved so far by the language that served them.
func (p *Provider) Served() map[string]int { return p.served }

func (p *Provider) gather(f *cache.File, pt geocode.Point, chain Languages) ([]Candidate, error) {
	var out []Candidate
	for _, i := range f.Candidates(pt.Lon, pt.Lat) {
		r := f.Rows[i]
		c := Candidate{ID: r.ID, Subtype: r.Subtype, Class: r.Class, Country: r.Country,
			AdminLevel: r.AdminLevel, Territorial: r.Territorial, Bbox: r.Bbox, row: i}
		c.Name, c.Language = nameOf(f, i, chain)
		g, err := f.Geometry(i)
		if err != nil {
			return nil, err
		}
		c.Contains, c.Distance = g.Locate(pt.Lon, pt.Lat)
		out = append(out, c)
	}
	return out, nil
}

func adminOrder(l int32) int64 {
	if l < 0 {
		return math.MaxInt32
	}
	return int64(l)
}

// countryBefore ranks containing country polygons: territorial first, smaller bbox, then id.
func countryBefore(a, b Candidate) bool {
	if a.Territorial != b.Territorial {
		return a.Territorial
	}
	if a.Area() != b.Area() {
		return a.Area() < b.Area()
	}
	return a.ID < b.ID
}

func rankCountry(cands []Candidate) int {
	best := -1
	for i, c := range cands {
		if c.Contains && (best < 0 || countryBefore(c, cands[best])) {
			best = i
		}
	}
	return best
}

func subtypeIndex(list []string, s string) int {
	s = strings.ToLower(s)
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return -1
}

// nameBefore ranks containing divisions: subtype order, lower admin level
// with unknown last, territorial first, bbox area, then id.
func nameBefore(a, b Candidate, list []string, largest bool) bool {
	if ia, ib := subtypeIndex(list, a.Subtype), subtypeIndex(list, b.Subtype); ia != ib {
		return ia < ib
	}
	if la, lb := adminOrder(a.AdminLevel), adminOrder(b.AdminLevel); la != lb {
		return la < lb
	}
	if a.Territorial != b.Territorial {
		return a.Territorial
	}
	if a.Area() != b.Area() {
		if largest {
			return a.Area() > b.Area()
		}
		return a.Area() < b.Area()
	}
	return a.ID < b.ID
}

func selectName(cands []Candidate, list []string, largest bool) int {
	best := -1
	for i, c := range cands {
		if !c.Contains || subtypeIndex(list, c.Subtype) < 0 {
			continue
		}
		if best < 0 || nameBefore(c, cands[best], list, largest) {
			best = i
		}
	}
	return best
}

// nearestBefore ranks non-containing divisions: subtype order, distance, then id.
func nearestBefore(a, b Candidate, list []string) bool {
	if ia, ib := subtypeIndex(list, a.Subtype), subtypeIndex(list, b.Subtype); ia != ib {
		return ia < ib
	}
	if a.Distance != b.Distance {
		return a.Distance < b.Distance
	}
	return a.ID < b.ID
}

// selectNearest is the fallback when nothing selectable contains the point:
// the nearest candidate of a listed subtype within bound.
func selectNearest(cands []Candidate, list []string, bound float64) int {
	best := -1
	for i, c := range cands {
		if c.Contains || c.Distance > bound || subtypeIndex(list, c.Subtype) < 0 {
			continue
		}
		if best < 0 || nearestBefore(c, cands[best], list) {
			best = i
		}
	}
	return best
}

func airportRankOf(c Candidate) int {
	if !strings.EqualFold(c.Subtype, "airport") {
		return 100
	}
	if r, ok := airportRank[strings.ToLower(c.Class)]; ok {
		return r
	}
	return 50
}

func centreDistance(c Candidate, pt geocode.Point) float64 {
	lon, lat := c.Bbox.Center()
	return geo.Haversine(pt.Lat, pt.Lon, lat, lon)
}

// airportBefore ranks containing airports: class rank, distance to the bbox centre, then id.
func airportBefore(a, b Candidate, pt geocode.Point) bool {
	if ra, rb := airportRankOf(a), airportRankOf(b); ra != rb {
		return ra < rb
	}
	if da, db := centreDistance(a, pt), centreDistance(b, pt); da != db {
		return da < db
	}
	return a.ID < b.ID
}

func selectAirport(cands []Candidate, pt geocode.Point) int {
	best := -1
	for i, c := range cands {
		if c.Contains && (best < 0 || airportBefore(c, cands[best], pt)) {
			best = i
		}
	}
	return best
}

// Explanation is the full calculation behind one result.
type Explanation struct {
	Point     geocode.Point     `json:"point"`
	Countries []Candidate       `json:"countries"`
	Code      string            `json:"code"`
	Profile   Profile           `json:"profile"`
	Divisions []Candidate       `json:"divisions"`
	Airports  []Candidate       `json:"airports"`
	State     string            `json:"state"`
	City      string            `json:"city"`
	Airport   string            `json:"airport"`
	Result    geocode.Result    `json:"result"`
	Releases  map[string]string `json:"releases"`
}

// Resolve implements geocode.Resolver. The city fallback is geocode's job.
func (p *Provider) Resolve(ctx context.Context, pt geocode.Point) (geocode.Result, error) {
	e, err := p.compute(ctx, pt)
	if err != nil {
		return geocode.Result{}, err
	}
	return e.Result, nil
}

// Explain is Resolve with every candidate and decision.
func (p *Provider) Explain(ctx context.Context, pt geocode.Point) (*Explanation, error) {
	return p.compute(ctx, pt)
}

// profile returns the catalog profile with the provider's optional override.
// Applying and normalizing a copy keeps both the catalogs and the override
// independent from resolution calls.
func (p *Provider) profile(code string) Profile {
	return p.Profiles.Profile(code).apply(p.Overrides).normalize()
}

// Country finds the country containing the point from world.geo alone.
func (p *Provider) Country(ctx context.Context, pt geocode.Point) (code, name string, found bool, err error) {
	w, err := p.World(ctx)
	if err != nil {
		return "", "", false, err
	}
	cands, err := p.gather(w, pt, Languages{LanguageEnglish})
	if err != nil {
		return "", "", false, err
	}
	i := rankCountry(cands)
	if i < 0 {
		return "", "", false, nil
	}
	return cands[i].Country, cands[i].Name, true, nil
}

// role labels a winner, marking the nearest-division fallback.
func role(r string, c Candidate) string {
	if c.Contains {
		return r
	}
	return r + ", nearest"
}

func decide(cands []Candidate, winner int, role string, lists ...[]string) {
	for i := range cands {
		c := &cands[i]
		switch {
		case c.Decision != "":
		case i == winner:
			c.Decision = role
		case !c.Contains:
			c.Decision = "bbox only"
		case len(lists) > 0 && !inLists(c.Subtype, lists):
			c.Decision = "contains, subtype not selectable"
		default:
			c.Decision = "contains, outranked"
		}
	}
}

func inLists(subtype string, lists [][]string) bool {
	for _, l := range lists {
		if subtypeIndex(l, subtype) >= 0 {
			return true
		}
	}
	return false
}

func (p *Provider) compute(ctx context.Context, pt geocode.Point) (*Explanation, error) {
	e := &Explanation{Point: pt, Releases: map[string]string{}}
	w, err := p.World(ctx)
	if err != nil {
		return nil, err
	}
	e.Releases["world"] = w.Header.Release
	e.Countries, err = p.gather(w, pt, Languages{LanguageEnglish})
	if err != nil {
		return nil, err
	}
	ci := rankCountry(e.Countries)
	decide(e.Countries, ci, "country")
	if ci < 0 || e.Countries[ci].Country == "" {
		return e, nil
	}
	e.Code = e.Countries[ci].Country
	e.Profile = p.profile(e.Code)
	e.Countries[ci].Name, e.Countries[ci].Language = nameOf(w, e.Countries[ci].row, p.chain(w, e.Profile.Language))
	country := e.Countries[ci]
	d, err := p.Divisions(ctx, e.Code)
	if err != nil {
		return nil, err
	}
	e.Releases["divisions/"+e.Code] = d.Header.Release
	e.Divisions, err = p.gather(d, pt, p.chain(d, e.Profile.Language))
	if err != nil {
		return nil, err
	}
	bound := *e.Profile.FallbackDistance
	si := selectName(e.Divisions, e.Profile.StateSubtypes, false)
	if si < 0 {
		si = selectNearest(e.Divisions, e.Profile.StateSubtypes, bound)
	}
	cityI := selectName(e.Divisions, e.Profile.PreferredSubtypes, e.Profile.TieBreakMode == TieBreakLargest)
	if cityI < 0 {
		cityI = selectNearest(e.Divisions, e.Profile.PreferredSubtypes, bound)
	}
	if si >= 0 {
		e.State = e.Divisions[si].Name
		e.Divisions[si].Decision = role("state", e.Divisions[si])
	}
	if cityI >= 0 {
		e.City = e.Divisions[cityI].Name
		if e.Divisions[cityI].Decision != "" {
			e.Divisions[cityI].Decision += ", city"
		} else {
			e.Divisions[cityI].Decision = role("city", e.Divisions[cityI])
		}
	}
	decide(e.Divisions, -1, "", e.Profile.StateSubtypes, e.Profile.PreferredSubtypes)
	if *e.Profile.Airports {
		a, err := p.Airports(ctx)
		if err != nil {
			return nil, err
		}
		e.Releases["airports"] = a.Header.Release
		e.Airports, err = p.gather(a, pt, p.chain(a, e.Profile.Language))
		if err != nil {
			return nil, err
		}
		ai := selectAirport(e.Airports, pt)
		if ai >= 0 {
			e.Airport = e.Airports[ai].Name
			if cityI >= 0 {
				e.Divisions[cityI].Decision += ", replaced by airport"
			}
		}
		decide(e.Airports, ai, "airport")
	}
	city, cityLang := e.City, ""
	if cityI >= 0 {
		cityLang = e.Divisions[cityI].Language
	}
	if e.Airport != "" {
		city = e.Airport
		for _, c := range e.Airports {
			if c.Name == e.Airport && strings.HasPrefix(c.Decision, "airport") {
				cityLang = c.Language
			}
		}
	}
	p.served[country.Language]++
	if si >= 0 {
		p.served[e.Divisions[si].Language]++
	}
	if city != "" {
		p.served[cityLang]++
	}
	e.Result = geocode.Result{City: city, State: e.State, Country: country.Name, Found: true}
	return e, nil
}

// SortedReleases returns the releases as "key release" lines, sorted.
func SortedReleases(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+" "+v)
	}
	sort.Strings(out)
	return out
}

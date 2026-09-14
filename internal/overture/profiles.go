package overture

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
)

//go:embed profiles.json
var bundledJSON []byte

// Tie-break modes for city selection.
const (
	TieBreakSmallest = "smallest-area"
	TieBreakLargest  = "largest-area"
)

// English and primary (local) name keys. Other language codes select translations.
const (
	LanguageEnglish = "en"
	LanguagePrimary = "primary"
)

// Languages is a name preference: Overture translation codes, en or primary,
// tried in order; en, primary and the id always follow. JSON accepts a
// string or a list.
type Languages []string

func (l *Languages) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*l = Languages{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("language: a string or a list of strings")
	}
	*l = many
	return nil
}

var languageCode = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{1,8})*$`)

// checkLanguages rejects codes that are neither primary nor language tags.
func checkLanguages(name string, l Languages) error {
	for _, code := range l {
		code = strings.TrimSpace(code)
		if code != "" && code != LanguagePrimary && !languageCode.MatchString(code) {
			return fmt.Errorf("%s: language %q is not a language tag or %s", name, code, LanguagePrimary)
		}
	}
	return nil
}

// hasLanguages reports whether the list names anything.
func hasLanguages(l Languages) bool {
	for _, code := range l {
		if strings.TrimSpace(code) != "" {
			return true
		}
	}
	return false
}

// normalizeLanguages trims, lower-cases the language subtag, drops blanks and
// repeats, and completes the chain with en and primary.
func normalizeLanguages(l Languages) Languages {
	out := Languages{}
	add := func(code string) {
		code = strings.TrimSpace(code)
		if code == "" {
			return
		}
		if code != LanguagePrimary {
			parts := strings.Split(code, "-")
			parts[0] = strings.ToLower(parts[0])
			code = strings.Join(parts, "-")
		}
		if !slices.Contains(out, code) {
			out = append(out, code)
		}
	}
	for _, code := range l {
		add(code)
	}
	add(LanguageEnglish)
	add(LanguagePrimary)
	return out
}

// DefaultFallbackDistance bounds nearest-division matching in planar degrees.
// Profiles can override it; zero disables the fallback.
const DefaultFallbackDistance = 0.01

var defaultSubtypes = []string{"locality", "borough", "localadmin", "macrohood", "neighborhood", "microhood"}

// defaultStateSubtypes is the state list unless a profile says otherwise.
var defaultStateSubtypes = []string{"region", "macroregion", "county", "macrocounty", "dependency"}

// divisionSubtypes are Overture's division subtypes, largest first.
var divisionSubtypes = []string{"country", "dependency", "region", "macroregion", "county", "macrocounty", "localadmin", "locality", "borough", "macrohood", "neighborhood", "microhood"}

// IsAlpha2 reports whether s is two ASCII letters, any case.
func IsAlpha2(s string) bool {
	return len(s) == 2 && isLetter(s[0]) && isLetter(s[1])
}

func isLetter(b byte) bool { return 'A' <= b && b <= 'Z' || 'a' <= b && b <= 'z' }

// Profile sets boundary preferences, languages and fallbacks for a country.
// Nil and empty fields inherit.
type Profile struct {
	PreferredSubtypes []string  `json:"preferredSubtypes"`
	TieBreakMode      string    `json:"tieBreakMode"`
	FallbackDistance  *float64  `json:"fallbackDistance,omitempty"` // planar degrees; 0 disables
	Airports          *bool     `json:"airports,omitempty"`
	Language          Languages `json:"language,omitempty"`
	StateSubtypes     []string  `json:"stateSubtypes"`
}

// Catalog is the profile file shape, bundled and user alike; keys are alpha-2.
type Catalog struct {
	DefaultProfile   Profile            `json:"defaultProfile"`
	CountryOverrides map[string]Profile `json:"countryOverrides"`
}

// Profiles merges the bundled catalog with an optional user catalog.
type Profiles struct {
	bundled Catalog
	user    Catalog
	path    string
}

// LoadProfiles reads the user catalog at path; an empty path means bundled
// only. Unknown keys, content after the catalog, override keys that are not
// alpha-2, unknown subtypes, unknown tie-break modes, negative distances and
// malformed languages are errors.
func LoadProfiles(path string) (*Profiles, error) {
	p := &Profiles{path: path}
	if err := decode(bundledJSON, &p.bundled); err != nil {
		return nil, fmt.Errorf("bundled profiles: %w", err)
	}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("profiles: %w", err)
		}
		if err := decode(b, &p.user); err != nil {
			return nil, fmt.Errorf("profiles %s: %w", path, err)
		}
	}
	return p, nil
}

func decode(b []byte, c *Catalog) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(c); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("content after the catalog")
	}
	subtypes := func(name, field string, list []string) error {
		for _, s := range list {
			if t := strings.ToLower(strings.TrimSpace(s)); t != "" && !slices.Contains(divisionSubtypes, t) {
				return fmt.Errorf("%s: %s %q is not a division subtype", name, field, s)
			}
		}
		return nil
	}
	check := func(name string, p Profile) error {
		if err := subtypes(name, "preferredSubtypes", p.PreferredSubtypes); err != nil {
			return err
		}
		if err := subtypes(name, "stateSubtypes", p.StateSubtypes); err != nil {
			return err
		}
		if m := strings.TrimSpace(p.TieBreakMode); m != "" && !strings.EqualFold(m, TieBreakSmallest) && !strings.EqualFold(m, TieBreakLargest) {
			return fmt.Errorf("%s: tieBreakMode %q is not %s or %s", name, p.TieBreakMode, TieBreakSmallest, TieBreakLargest)
		}
		if p.FallbackDistance != nil && *p.FallbackDistance < 0 {
			return fmt.Errorf("%s: fallbackDistance %g is negative", name, *p.FallbackDistance)
		}
		return checkLanguages(name, p.Language)
	}
	if err := check("defaultProfile", c.DefaultProfile); err != nil {
		return err
	}
	for code, o := range c.CountryOverrides {
		if !IsAlpha2(code) {
			return fmt.Errorf("countryOverrides: %q is not an alpha-2 code", code)
		}
		if err := check(code, o); err != nil {
			return err
		}
	}
	return nil
}

// Profile returns the effective profile for a country: bundled default,
// bundled country, user default, user country; normalized.
func (p *Profiles) Profile(code string) Profile {
	code = strings.ToUpper(code)
	eff := p.bundled.DefaultProfile
	eff = eff.apply(lookup(p.bundled.CountryOverrides, code))
	eff = eff.apply(p.user.DefaultProfile)
	eff = eff.apply(lookup(p.user.CountryOverrides, code))
	return eff.normalize()
}

func lookup(m map[string]Profile, code string) Profile {
	for k, v := range m {
		if strings.ToUpper(k) == code {
			return v
		}
	}
	return Profile{}
}

// apply merges non-empty lists, a non-blank tie-break mode and non-nil fields.
func (p Profile) apply(o Profile) Profile {
	if len(o.PreferredSubtypes) > 0 {
		p.PreferredSubtypes = slices.Clone(o.PreferredSubtypes)
	}
	if len(o.StateSubtypes) > 0 {
		p.StateSubtypes = slices.Clone(o.StateSubtypes)
	}
	if strings.TrimSpace(o.TieBreakMode) != "" {
		p.TieBreakMode = o.TieBreakMode
	}
	if o.FallbackDistance != nil {
		d := *o.FallbackDistance
		p.FallbackDistance = &d
	}
	if o.Airports != nil {
		b := *o.Airports
		p.Airports = &b
	}
	if hasLanguages(o.Language) {
		p.Language = slices.Clone(o.Language)
	}
	return p
}

// normalize cleans subtype lists, canonicalizes the tie-break mode,
// completes the language chain and fills missing defaults.
func (p Profile) normalize() Profile {
	p.PreferredSubtypes = normalizeSubtypes(p.PreferredSubtypes, defaultSubtypes)
	p.StateSubtypes = normalizeSubtypes(p.StateSubtypes, defaultStateSubtypes)
	if strings.EqualFold(strings.TrimSpace(p.TieBreakMode), TieBreakLargest) {
		p.TieBreakMode = TieBreakLargest
	} else {
		p.TieBreakMode = TieBreakSmallest
	}
	if p.FallbackDistance == nil {
		d := DefaultFallbackDistance
		p.FallbackDistance = &d
	}
	if p.Airports == nil {
		b := true
		p.Airports = &b
	}
	p.Language = normalizeLanguages(p.Language)
	return p
}

func normalizeSubtypes(list, def []string) []string {
	var out []string
	for _, s := range list {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return slices.Clone(def)
	}
	return out
}

// Source names the catalogs in use.
func (p *Profiles) Source() string {
	if p.path == "" {
		return "bundled"
	}
	return "bundled+" + p.path
}

// Default is the effective profile of a country without an override.
func (p *Profiles) Default() Profile {
	return p.bundled.DefaultProfile.apply(p.user.DefaultProfile).normalize()
}

// Codes lists every country with an override, sorted.
func (p *Profiles) Codes() []string {
	codes := map[string]bool{}
	for k := range p.bundled.CountryOverrides {
		codes[strings.ToUpper(k)] = true
	}
	for k := range p.user.CountryOverrides {
		codes[strings.ToUpper(k)] = true
	}
	keys := make([]string, 0, len(codes))
	for k := range codes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Describe puts the source, the effective default and every override on one
// line, for the dry-run header.
func (p *Profiles) Describe() string {
	parts := []string{"default=" + p.Default().String()}
	for _, k := range p.Codes() {
		parts = append(parts, k+"="+p.Profile(k).String())
	}
	return p.Source() + " " + strings.Join(parts, "; ")
}

func (p Profile) String() string {
	s := "[" + strings.Join(p.PreferredSubtypes, " ") + "] " + p.TieBreakMode
	if len(p.StateSubtypes) > 0 {
		s += " state=[" + strings.Join(p.StateSubtypes, " ") + "]"
	}
	if p.FallbackDistance != nil {
		s += fmt.Sprintf(" fallback=%g", *p.FallbackDistance)
	}
	if p.Airports != nil {
		s += fmt.Sprintf(" airports=%t", *p.Airports)
	}
	if len(p.Language) > 0 {
		s += " language=" + strings.Join(p.Language, ",")
	}
	return s
}

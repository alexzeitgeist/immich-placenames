// Command immich-placenames names Immich assets from Overture division polygons.
package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/alexzeitgeist/immich-placenames/internal/cache"
	"github.com/alexzeitgeist/immich-placenames/internal/geo"
	"github.com/alexzeitgeist/immich-placenames/internal/geocode"
	"github.com/alexzeitgeist/immich-placenames/internal/immich"
	"github.com/alexzeitgeist/immich-placenames/internal/overture"
	"github.com/alexzeitgeist/immich-placenames/internal/overture/fetch"
)

// command defines a subcommand's usage and handler. The handler registers its flags.
type command struct {
	name, synopsis string
	db             bool
	run            func(*app, context.Context, *flag.FlagSet, []string) error
}

// Flags precede operands, as the standard parser reads them.
var commands = []command{
	{"fetch", "[-data DIR] [-workers N] [-release R] [-areas=BOOL] [-points] world | airports | CC...", false, (*app).fetch},
	{"lookup", "[-data DIR] [-profiles FILE] [-workers N] [-json] [-airports=BOOL] [-fallback-distance N] [-points=BOOL] [-point-distance N] LAT LON", false, (*app).lookup},
	{"run", "[-data DIR] [-profiles FILE] [-workers N] [-dry-run] [-all] [-lock] [-limit N] [-page-size N]", true, (*app).run},
	{"status", "[-data DIR] [-profiles FILE]", true, (*app).status},
	{"reset", "[-dry-run] -all | -ids FILE | -city V | -state V | -country V", true, (*app).reset},
	{"version", "", false, (*app).version},
}

const dbText = "Database: DB_URL, or DB_HOSTNAME (database), DB_PORT (5432), DB_USERNAME, DB_PASSWORD, DB_DATABASE_NAME.\n"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	a := &app{data: "data", workers: 4, log: log, out: os.Stdout, errOut: os.Stderr}
	if len(os.Args) < 2 {
		a.usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "-h", "-help", "--help":
		a.usage()
		os.Exit(0)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := a.dispatch(ctx, os.Args[1], os.Args[2:])
	stop()
	var usage usageError
	switch {
	case err == nil:
	case errors.Is(err, flag.ErrHelp):
		os.Exit(0)
	case errors.As(err, &usage):
		os.Exit(2)
	default:
		a.log.Error(err.Error())
		os.Exit(1)
	}
}

// usage prints the command table.
func (a *app) usage() {
	fmt.Fprint(a.errOut, "usage: immich-placenames COMMAND [-v] [flags] [args]; flags precede args\n\n")
	for _, c := range commands {
		fmt.Fprintf(a.errOut, "  %s\n", strings.TrimSpace(fmt.Sprintf("%-8s %s", c.name, c.synopsis)))
	}
	fmt.Fprint(a.errOut, "\n-data defaults to data; -profiles to DATA/profiles.json when present, else the bundled catalog alone.\n"+dbText)
}

// usageError is a command-line error, already reported with the usage text;
// exit status 2.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// usagef reports an argument error with the command's usage.
func usagef(fs *flag.FlagSet, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintln(fs.Output(), msg)
	fs.Usage()
	return usageError{msg}
}

// dispatch runs the named command with its own flag set. help prints the
// command table; help COMMAND is COMMAND -h.
func (a *app) dispatch(ctx context.Context, name string, args []string) error {
	if name == "help" {
		if len(args) == 0 {
			a.usage()
			return flag.ErrHelp
		}
		name, args = args[0], []string{"-h"}
	}
	for _, c := range commands {
		if c.name == name {
			return c.run(a, ctx, a.flagSet(c), args)
		}
	}
	fmt.Fprintf(a.errOut, "unknown command %q\n", name)
	a.usage()
	return usageError{"unknown command " + name}
}

// flagSet is a command's parser. Its usage prints the synopsis, the flag
// defaults and, for database commands, the connection variables.
func (a *app) flagSet(c command) *flag.FlagSet {
	fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
	fs.SetOutput(a.errOut)
	fs.BoolVar(&a.verbose, "v", false, "enable debug logging on stderr")
	fs.Usage = func() {
		fmt.Fprintf(a.errOut, "usage: immich-placenames %s\n", strings.TrimSpace(c.name+" [-v] "+c.synopsis))
		fs.PrintDefaults()
		if c.db {
			fmt.Fprint(a.errOut, dbText)
		}
	}
	return fs
}

// parse reads a command's flags. The standard parser has reported a bad flag
// or answered -h; -workers, where defined, is checked before any network work.
func (a *app) parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError{err.Error()}
	}
	level := slog.LevelInfo
	if a.verbose {
		level = slog.LevelDebug
	}
	a.log = slog.New(slog.NewTextHandler(a.errOut, &slog.HandlerOptions{Level: level}))
	if fs.Lookup("workers") != nil && a.workers < 1 {
		return usagef(fs, "-workers must be at least 1")
	}
	return nil
}

var buildRevision string

func version() string {
	rev, modified := buildRevision, ""
	if rev == "" {
		bi, ok := debug.ReadBuildInfo()
		if !ok {
			return "unknown"
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				if s.Value == "true" {
					modified = "-modified"
				}
			}
		}
		if rev == "" {
			return "devel"
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	return rev + modified
}

type app struct {
	data, profilesPath string
	workers            int
	verbose            bool
	log                *slog.Logger
	out, errOut        io.Writer
}

// The flags several commands share, each registered where it applies.
func (a *app) dataFlag(fs *flag.FlagSet) { fs.StringVar(&a.data, "data", a.data, "data directory") }
func (a *app) profilesFlag(fs *flag.FlagSet) {
	fs.StringVar(&a.profilesPath, "profiles", "", "profile catalog; default DATA/profiles.json when present")
}
func (a *app) workersFlag(fs *flag.FlagSet) {
	fs.IntVar(&a.workers, "workers", a.workers, "parallel row-group readers when fetching")
}

// profiles loads -profiles or DATA/profiles.json over the bundled catalog.
// Without either file, it uses the bundled catalog alone.
func (a *app) profiles() (*overture.Profiles, error) {
	path := a.profilesPath
	if path == "" {
		if p := filepath.Join(a.data, "profiles.json"); exists(p) {
			path = p
		}
	}
	return overture.LoadProfiles(path)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// fetcher chooses the explicit release, then world.geo's release, then latest.
func (a *app) fetcher(release string) *fetcher {
	return &fetcher{c: &fetch.Client{Workers: a.workers, Log: a.log}, release: release, world: overture.WorldPath(a.data), log: a.log}
}

// provider wires the resolver with its profiles and on-demand fetching at
// world.geo's release.
func (a *app) provider() (*overture.Provider, error) {
	profiles, err := a.profiles()
	if err != nil {
		return nil, err
	}
	return overture.New(a.data, profiles, a.fetcher(""), a.log), nil
}

// fetcher resolves the release once and builds cache files through
// fetch.Client.
type fetcher struct {
	c                 *fetch.Client
	release, resolved string
	world             string
	log               *slog.Logger
}

func (f *fetcher) Release(ctx context.Context) (string, error) {
	if f.resolved != "" {
		return f.resolved, nil
	}
	r := f.release
	if r == "" {
		r = f.worldRelease()
	}
	if r == "" || r == "latest" {
		var err error
		if r, err = f.c.LatestRelease(ctx); err != nil {
			return "", fmt.Errorf("latest release: %w", err)
		}
		f.log.Info("latest release", "release", r)
	}
	f.resolved = r
	return r, nil
}

// worldRelease is the installed country polygons' release, "" when absent.
func (f *fetcher) worldRelease() string {
	w, err := cache.Open(f.world)
	if err != nil {
		return ""
	}
	defer w.Close()
	return w.Header.Release
}

func (f *fetcher) World(ctx context.Context, release, dest string) error {
	if release == "" {
		var err error
		if release, err = f.Release(ctx); err != nil {
			return err
		}
	}
	_, err := f.c.World(ctx, release, dest)
	return err
}

func (f *fetcher) Divisions(ctx context.Context, code string, box geo.Bbox, dest string) error {
	r, err := f.Release(ctx)
	if err != nil {
		return err
	}
	_, err = f.c.Divisions(ctx, r, code, box, dest)
	return err
}

func (f *fetcher) Points(ctx context.Context, code string, box geo.Bbox, dest string) error {
	r, err := f.Release(ctx)
	if err != nil {
		return err
	}
	_, err = f.c.Points(ctx, r, code, box, dest)
	return err
}

func (f *fetcher) Airports(ctx context.Context, dest string) error {
	r, err := f.Release(ctx)
	if err != nil {
		return err
	}
	_, err = f.c.Airports(ctx, r, dest)
	return err
}

// fetch needs no profiles: the provider serves only the country bbox from
// world.geo.
func (a *app) fetch(ctx context.Context, fs *flag.FlagSet, args []string) error {
	a.dataFlag(fs)
	a.workersFlag(fs)
	release := fs.String("release", "latest", "Overture release")
	areas := fs.Bool("areas", true, "fetch the division areas of the listed countries")
	points := fs.Bool("points", false, "also fetch their division points")
	if err := a.parse(fs, args); err != nil {
		return err
	}
	world, airports, codes, err := fetchTargets(fs.Args())
	if err != nil {
		return usagef(fs, "%v", err)
	}
	if len(codes) > 0 && !*areas && !*points {
		return usagef(fs, "fetch: -areas=false leaves nothing to fetch for %s without -points", strings.Join(codes, ", "))
	}
	f := a.fetcher(*release)
	p := overture.New(a.data, nil, f, a.log)
	defer p.Close()
	if world {
		if err := f.World(ctx, "", overture.WorldPath(a.data)); err != nil {
			return err
		}
	}
	boxes := make([]geo.Bbox, len(codes))
	for i, code := range codes {
		if boxes[i], err = p.CountryBbox(ctx, code); err != nil {
			return err
		}
	}
	for i, code := range codes {
		if *areas {
			if err := f.Divisions(ctx, code, boxes[i], overture.DivisionsPath(a.data, code)); err != nil {
				return err
			}
		}
		if *points {
			if err := f.Points(ctx, code, boxes[i], overture.PointsPath(a.data, code)); err != nil {
				return err
			}
		}
	}
	if airports {
		return f.Airports(ctx, overture.AirportsPath(a.data))
	}
	return nil
}

// fetchTargets separates world and airports from deduplicated, uppercase codes.
// It checks code syntax; country existence is checked against world.geo.
func fetchTargets(args []string) (world, airports bool, codes []string, err error) {
	for _, t := range args {
		switch {
		case strings.EqualFold(t, "world"):
			world = true
		case strings.EqualFold(t, "airports"):
			airports = true
		case !overture.IsAlpha2(t):
			return false, false, nil, fmt.Errorf("fetch: %q is not an alpha-2 code", t)
		default:
			if code := strings.ToUpper(t); !slices.Contains(codes, code) {
				codes = append(codes, code)
			}
		}
	}
	if !world && !airports && len(codes) == 0 {
		return false, false, nil, errors.New("fetch: world, airports or country codes required")
	}
	return world, airports, codes, nil
}

func (a *app) lookup(ctx context.Context, fs *flag.FlagSet, args []string) error {
	a.dataFlag(fs)
	a.profilesFlag(fs)
	a.workersFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
	// Only explicit flags override the profile. Zero defaults keep inherited
	// settings out of PrintDefaults.
	airports := fs.Bool("airports", false, "airport matching override; omitted inherits the profile")
	distance := fs.Float64("fallback-distance", 0, "nearest-division bound in degrees; 0 disables, omitted inherits the profile")
	points := fs.Bool("points", false, "division-point fallback override; omitted inherits the profile")
	pointDistance := fs.Float64("point-distance", 0, "division-point bound in metres; 0 disables, omitted inherits the profile")
	// Keep the final two arguments out of flag parsing to allow negative coordinates.
	// With fewer arguments, parse still handles -h.
	flags, coords := args, []string(nil)
	if len(args) >= 2 {
		flags, coords = args[:len(args)-2], args[len(args)-2:]
	}
	if err := a.parse(fs, flags); err != nil {
		return err
	}
	if *distance < 0 || math.IsNaN(*distance) || math.IsInf(*distance, 0) {
		return usagef(fs, "-fallback-distance must be finite and non-negative")
	}
	if *pointDistance < 0 || math.IsNaN(*pointDistance) || math.IsInf(*pointDistance, 0) {
		return usagef(fs, "-point-distance must be finite and non-negative")
	}
	if coords == nil || fs.NArg() != 0 {
		return usagef(fs, "lookup: LAT LON must be the last two arguments")
	}
	lat, err1 := strconv.ParseFloat(coords[0], 64)
	lon, err2 := strconv.ParseFloat(coords[1], 64)
	if err1 != nil || err2 != nil {
		return usagef(fs, "lookup: LAT LON must be decimal degrees")
	}
	pt := geocode.Point{Lat: lat, Lon: lon}
	if !pt.Valid() {
		return usagef(fs, "lookup: LAT within ±90 and LON within ±180 required")
	}
	p, err := a.provider()
	if err != nil {
		return err
	}
	defer p.Close()
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "airports":
			p.Overrides.Airports = airports
		case "fallback-distance":
			p.Overrides.FallbackDistance = distance
		case "points":
			p.Overrides.PointFallback = points
		case "point-distance":
			p.Overrides.PointDistance = pointDistance
		}
	})
	e, err := p.Explain(ctx, pt)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(a.out)
		enc.SetIndent("", "  ")
		return enc.Encode(e)
	}
	printExplanation(a.out, e)
	return nil
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func printExplanation(out io.Writer, e *overture.Explanation) {
	w := bufio.NewWriter(out)
	defer w.Flush()
	fmt.Fprintf(w, "point %.7f %.7f\n", e.Point.Lat, e.Point.Lon)
	fmt.Fprintf(w, "world.geo %s: %d bbox candidates\n", e.Releases["world"], len(e.Countries))
	for _, c := range e.Countries {
		printCandidate(w, c, false)
	}
	if e.Code == "" {
		fmt.Fprintln(w, "result: no country")
		return
	}
	fmt.Fprintf(w, "divisions/%s.geo %s: profile %s; %d bbox candidates\n", e.Code, e.Releases["divisions/"+e.Code], e.Profile, len(e.Divisions))
	for _, c := range e.Divisions {
		printCandidate(w, c, false)
	}
	if r, ok := e.Releases["points/"+e.Code]; ok {
		fmt.Fprintf(w, "points/%s.geo %s: %d label candidates\n", e.Code, r, len(e.Points))
		for _, c := range e.Points {
			printCandidate(w, c, true)
		}
	}
	if r, ok := e.Releases["airports"]; ok {
		fmt.Fprintf(w, "airports.geo %s: %d bbox candidates\n", r, len(e.Airports))
		for _, c := range e.Airports {
			printCandidate(w, c, false)
		}
	} else {
		fmt.Fprintln(w, "airports off")
	}
	fmt.Fprintf(w, "state: %s\ncity: %s\nairport: %s\n", or(e.State, "-"), or(e.City, "-"), or(e.Airport, "-"))
	if e.CountryReplaced != "" {
		fmt.Fprintf(w, "country: %s to %s\n", e.CountryReplaced, or(e.Result.Country, "-"))
	}
	if e.Overridden != "" {
		city := e.Result.City
		if e.CityFilledFrom != "" {
			// A fallback after an override means the override cleared the city.
			city = ""
		}
		fmt.Fprintf(w, "override: %s to %s\n", e.Overridden, or(city, "-"))
	}
	if e.CityFilledFrom != "" {
		fmt.Fprintf(w, "city fallback: %s\n", e.CityFilledFrom)
	}
	fmt.Fprintf(w, "result: %s, %s, %s\n", e.Result.City, e.Result.State, e.Result.Country)
}

// printCandidate prints area in 1e-4 square degrees, distance and name language.
// Label distances use metres; area distances use degrees.
func printCandidate(w io.Writer, c overture.Candidate, label bool) {
	in := "bbox"
	switch {
	case c.Contains:
		in = "IN  "
	case label:
		in = "near"
	}
	lvl := "-"
	if c.AdminLevel >= 0 {
		lvl = strconv.Itoa(int(c.AdminLevel))
	}
	terr := " "
	if c.Territorial {
		terr = "T"
	}
	dist := strings.Repeat(" ", 12)
	switch {
	case label:
		dist = fmt.Sprintf("dist=%6.0fm", c.Metres)
	case !c.Contains:
		dist = fmt.Sprintf("dist=%.5f", c.Distance)
	}
	fmt.Fprintf(w, "  %s %-12s %-22s lvl=%-2s %s area=%9.2f %s  %-40s %s\n", in, c.Subtype, c.Class, lvl, terr, c.Area()*1e4, dist, c.Name+" ("+c.Language+")", c.Decision)
}

// connect opens the database the environment names.
func (a *app) connect(ctx context.Context) (*pgx.Conn, immich.Config, error) {
	cfg, err := immich.ConfigFromEnv()
	if err != nil {
		return nil, cfg, err
	}
	conn, err := immich.Connect(ctx, cfg.DSN())
	if err != nil {
		return nil, cfg, fmt.Errorf("database %s:%s: %w", cfg.Host, cfg.Port, err)
	}
	return conn, cfg, nil
}

// run validates profiles before connecting to the database.
func (a *app) run(ctx context.Context, fs *flag.FlagSet, args []string) error {
	a.dataFlag(fs)
	a.profilesFlag(fs)
	a.workersFlag(fs)
	dryRun := fs.Bool("dry-run", false, "print the selection as CSV instead of writing; missing caches are still fetched")
	all := fs.Bool("all", false, "every asset with coordinates, not only unnamed ones")
	lock := fs.Bool("lock", false, "also lock the three columns against metadata extraction")
	limit := fs.Int("limit", 0, "first N assets in created order across the whole run")
	pageSize := fs.Int("page-size", defaultPageSize, "selected assets per transaction; 0 uses one transaction for the whole run")
	if err := a.parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return usagef(fs, "run takes no arguments")
	}
	if *limit < 0 {
		return usagef(fs, "-limit must not be negative")
	}
	if *pageSize < 0 {
		return usagef(fs, "-page-size must not be negative")
	}
	p, err := a.provider()
	if err != nil {
		return err
	}
	defer p.Close()
	db, _, err := a.connect(ctx)
	if err != nil {
		return err
	}
	defer db.Close(context.Background())
	opts := runOptions{All: *all, Lock: *lock, DryRun: *dryRun, Limit: *limit, PageSize: *pageSize}
	a.log.Info("pass started", "all", opts.All, "limit", opts.Limit, "page_size", opts.PageSize)
	report, runErr := a.processRun(ctx, databaseStore{db}, p, opts)
	releases := p.Releases()
	if distinct(releases) > 1 {
		a.log.Warn("mixed cache releases", "caches", strings.Join(overture.SortedReleases(releases), ", "))
	}
	if runErr == nil && opts.DryRun {
		runErr = printDryRun(a.out, report.Updates, releases, p.Profiles.Describe(), p.Served(), p.Filled(), opts.All, report.Selected)
		if runErr == nil {
			report.Reported = len(report.Updates)
		}
	}
	message := "pass complete"
	if runErr != nil {
		message = "pass stopped"
	}
	a.log.Info(message, "selected", report.Selected, "written", report.Written, "reported", report.Reported,
		"no_country", report.NoCountry, "changed", report.Changed, "overridden", p.Overridden(),
		"city_fallback", total(p.Filled()), "errors", report.Failed, "committed_pages", report.CommittedPages)
	if runErr != nil {
		if opts.DryRun {
			return fmt.Errorf("dry-run stopped: %w", runErr)
		}
		return fmt.Errorf("stopped after %d confirmed writes in %d committed pages: %w", report.Written, report.CommittedPages, runErr)
	}
	if report.Failed > 0 {
		return fmt.Errorf("%d assets failed", report.Failed)
	}
	return nil
}

func total(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func distinct(m map[string]string) int {
	seen := map[string]bool{}
	for _, v := range m {
		seen[v] = true
	}
	return len(seen)
}

// printDryRun writes metadata, per-asset CSV rows and a grouped summary.
// served counts names by language; filled counts city fallbacks by source.
func printDryRun(out io.Writer, updates []immich.Update, releases map[string]string, profiles string, served, filled map[string]int, all bool, selected int) error {
	w := bufio.NewWriter(out)
	fmt.Fprintf(w, "# immich-placenames %s\n", version())
	for _, r := range overture.SortedReleases(releases) {
		fmt.Fprintf(w, "# cache %s\n", r)
	}
	fmt.Fprintf(w, "# profiles %s\n", profiles)
	printCounts(w, "names", served)
	printCounts(w, "fallback", filled)
	sel := "unprocessed"
	if all {
		sel = "all"
	}
	fmt.Fprintf(w, "# selection %s assets=%d resolved=%d\n", sel, selected, len(updates))
	sort.Slice(updates, func(i, j int) bool { return updates[i].ID < updates[j].ID })
	cw := csv.NewWriter(w)
	cw.Write([]string{"assetId", "city", "state", "country"})
	for _, u := range updates {
		cw.Write([]string{u.ID, u.City, u.State, u.Country})
	}
	cw.Flush()
	fmt.Fprintln(w)
	type key struct{ country, state, city string }
	counts := map[key]int{}
	for _, u := range updates {
		counts[key{u.Country, u.State, u.City}]++
	}
	keys := make([]key, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.country != b.country {
			return a.country < b.country
		}
		if a.state != b.state {
			return a.state < b.state
		}
		return a.city < b.city
	})
	cw.Write([]string{"country", "state", "city", "assets"})
	for _, k := range keys {
		cw.Write([]string{k.country, k.state, k.city, strconv.Itoa(counts[k])})
	}
	cw.Flush()
	return errors.Join(cw.Error(), w.Flush())
}

// printCounts writes a comment line of key=count pairs, most frequent first.
func printCounts(w io.Writer, label string, counts map[string]int) {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	fmt.Fprint(w, "# "+label)
	for _, k := range keys {
		fmt.Fprintf(w, " %s=%d", k, counts[k])
	}
	fmt.Fprintln(w)
}

// status lists the caches, the profile source with one line per effective
// profile, then the database and its unprocessed count.
func (a *app) status(ctx context.Context, fs *flag.FlagSet, args []string) error {
	a.dataFlag(fs)
	a.profilesFlag(fs)
	if err := a.parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return usagef(fs, "status takes no arguments")
	}
	printCaches(a.out, a.data)
	profiles, err := a.profiles()
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "profiles %s\n  %-8s %s\n", profiles.Source(), "default", profiles.Default())
	for _, c := range profiles.Codes() {
		fmt.Fprintf(a.out, "  %-8s %s\n", c, profiles.Profile(c))
	}
	db, cfg, err := a.connect(ctx)
	if err != nil {
		return err
	}
	defer db.Close(context.Background())
	fmt.Fprintf(a.out, "database %s:%s/%s\n", cfg.Host, cfg.Port, cfg.Database)
	n, err := immich.Unprocessed(ctx, db)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "unprocessed assets %d\n", n)
	return nil
}

// printCaches lists every cache file under dir, then stale temporary files.
func printCaches(out io.Writer, dir string) {
	paths := []string{overture.WorldPath(dir), overture.AirportsPath(dir)}
	for _, sub := range []string{"divisions", "points"} {
		m, _ := filepath.Glob(filepath.Join(dir, sub, "*.geo"))
		sort.Strings(m)
		paths = append(paths, m...)
	}
	for _, path := range paths {
		rel, _ := filepath.Rel(dir, path)
		f, err := cache.Open(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			fmt.Fprintf(out, "%-18s absent\n", rel)
		case err != nil:
			fmt.Fprintf(out, "%-18s invalid: %v\n", rel, err)
		default:
			names := "en"
			if f.HasPrimary {
				names += "+primary"
			}
			if f.HasCommon {
				names += fmt.Sprintf("+common(%d languages)", len(f.Languages()))
			}
			note := ""
			if f.Header.Kind == "world" && !f.Header.Dependencies {
				note = " without dependency territories"
			}
			fmt.Fprintf(out, "%-18s release %s rows %d size %d MB fetched %s names %s%s\n", rel, f.Header.Release, len(f.Rows), f.Size()/1e6, f.Header.Fetched.Format(time.RFC3339), names, note)
			f.Close()
		}
	}
	for _, pattern := range []string{filepath.Join(dir, "*", "*.geo.tmp-*"), filepath.Join(dir, "*.geo.tmp-*")} {
		if stale, _ := filepath.Glob(pattern); len(stale) > 0 {
			fmt.Fprintf(out, "stale temporary files: %s\n", strings.Join(stale, " "))
		}
	}
}

func (a *app) reset(ctx context.Context, fs *flag.FlagSet, args []string) error {
	dryRun := fs.Bool("dry-run", false, "count the rows instead of clearing them")
	all := fs.Bool("all", false, "every row with names or locks")
	ids := fs.String("ids", "", "file with one asset id per line")
	city := fs.String("city", "", "rows whose city equals V")
	state := fs.String("state", "", "rows whose state equals V")
	country := fs.String("country", "", "rows whose country equals V")
	if err := a.parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return usagef(fs, "reset takes no arguments")
	}
	set := 0
	for _, on := range []bool{*all, *ids != "", *city != "", *state != "", *country != ""} {
		if on {
			set++
		}
	}
	if set != 1 {
		return usagef(fs, "reset: exactly one of -all, -ids, -city, -state, -country")
	}
	var sel immich.Selection
	var err error
	switch {
	case *all:
		sel = immich.All()
	case *ids != "":
		list, err := readIDs(*ids)
		if err != nil {
			return err
		}
		sel = immich.IDs(list)
	case *city != "":
		sel, err = immich.Named("city", *city)
	case *state != "":
		sel, err = immich.Named("state", *state)
	default:
		sel, err = immich.Named("country", *country)
	}
	if err != nil {
		return err
	}
	db, _, err := a.connect(ctx)
	if err != nil {
		return err
	}
	defer db.Close(context.Background())
	if *dryRun {
		n, err := immich.Count(ctx, db, sel)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.out, "would reset %d rows\n", n)
		return nil
	}
	n, err := immich.Reset(ctx, db, sel)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "reset %d rows\n", n)
	return nil
}

func readIDs(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out, nil
}

func (a *app) version(ctx context.Context, fs *flag.FlagSet, args []string) error {
	if err := a.parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return usagef(fs, "version takes no arguments")
	}
	fmt.Fprintf(a.out, "immich-placenames %s\n", version())
	return nil
}

package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/alexzeitgeist/immich-placenames/internal/cache"
	"github.com/alexzeitgeist/immich-placenames/internal/overture"
	"github.com/alexzeitgeist/immich-placenames/internal/overture/fetch"
)

// On-demand fetches follow the installed country polygons' release; an
// explicit release wins.
func TestReleaseFollowsWorld(t *testing.T) {
	dir := t.TempDir()
	w, err := cache.NewWriter(overture.WorldPath(dir), cache.Header{Kind: "world", Release: "2026-01-01.0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f := &fetcher{c: &fetch.Client{}, world: overture.WorldPath(dir), log: log}
	if r, err := f.Release(context.Background()); err != nil || r != "2026-01-01.0" {
		t.Fatalf("got %q, %v", r, err)
	}
	f = &fetcher{c: &fetch.Client{}, release: "2026-02-02.0", world: overture.WorldPath(dir), log: log}
	if r, err := f.Release(context.Background()); err != nil || r != "2026-02-02.0" {
		t.Fatalf("explicit release: got %q, %v", r, err)
	}
}

func quiet(dir string) *app {
	return &app{data: dir, workers: 4, log: slog.New(slog.NewTextHandler(io.Discard, nil)), out: io.Discard, errOut: io.Discard}
}

// call runs one command line as main does.
func (a *app) call(args ...string) error { return a.dispatch(context.Background(), args[0], args[1:]) }

func isUsage(err error) bool {
	var u usageError
	return errors.As(err, &u)
}

// Each command parses its own flags after its name and nothing else; run
// loads the profiles before it touches the database.
func TestFlagsPerCommand(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "none.json")
	t.Setenv("DB_USERNAME", "")
	a := quiet(t.TempDir())
	if err := a.call("lookup", "-profiles", missing, "-json", "42.65", "18.07"); !errors.Is(err, os.ErrNotExist) || a.profilesPath != missing {
		t.Errorf("lookup -profiles: %v, path %q", err, a.profilesPath)
	}
	if err := a.call("run", "-dry-run", "-all", "-profiles", missing, "-workers", "2"); !errors.Is(err, os.ErrNotExist) || a.workers != 2 {
		t.Errorf("run -profiles: %v, workers %d", err, a.workers)
	}
	if err := a.call("run"); err == nil || !strings.Contains(err.Error(), "DB_USERNAME") {
		t.Errorf("run without a profile file: %v", err)
	}
	for _, args := range [][]string{
		{"fetch", "-profiles", missing, "world"},
		{"status", "-workers", "2"},
		{"reset", "-data", "x", "-all"},
		{"reset", "-profiles", missing, "-all"},
		{"reset", "-workers", "2", "-all"},
		{"bogus"},
	} {
		if err := a.call(args...); !isUsage(err) {
			t.Errorf("%v: %v", args, err)
		}
	}
}

// -h works without operands and includes connection variables for database commands.
func TestHelp(t *testing.T) {
	a := quiet(t.TempDir())
	var out strings.Builder
	a.errOut = &out
	for _, c := range commands {
		out.Reset()
		if err := a.call(c.name, "-h"); !errors.Is(err, flag.ErrHelp) || !strings.Contains(out.String(), "usage: immich-placenames "+c.name) || c.db != strings.Contains(out.String(), "DB_USERNAME") {
			t.Errorf("%s -h: %v; printed %q", c.name, err, out.String())
		}
	}
	if err := a.call("lookup", "--help"); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("lookup --help: %v", err)
	}
	out.Reset()
	if err := a.call("help", "lookup"); !errors.Is(err, flag.ErrHelp) || !strings.Contains(out.String(), "usage: immich-placenames lookup") {
		t.Errorf("help lookup: %v; printed %q", err, out.String())
	}
	out.Reset()
	if err := a.call("help"); !errors.Is(err, flag.ErrHelp) || !strings.Contains(out.String(), "usage: immich-placenames COMMAND") {
		t.Errorf("help: %v; printed %q", err, out.String())
	}
	if err := a.call("help", "bogus"); !isUsage(err) {
		t.Errorf("help bogus: %v", err)
	}
	out.Reset()
	a.usage()
	for _, c := range commands {
		if !strings.Contains(out.String(), "\n  "+c.name) {
			t.Errorf("usage lacks %s: %q", c.name, out.String())
		}
	}
}

// Reject unused operands. lookup requires exactly two final coordinates.
func TestOperandsRejected(t *testing.T) {
	a := quiet(t.TempDir())
	for _, args := range [][]string{
		{"run", "extra"}, {"reset", "-all", "extra"}, {"status", "extra"}, {"version", "extra"},
		{"lookup", "1"}, {"lookup", "42", "18", "-json"},
		{"reset"}, {"reset", "-all", "-city", "X"},
		{"fetch"}, {"fetch", "-airports", "HR"}, {"fetch", "HRV"},
	} {
		if err := a.call(args...); !isUsage(err) {
			t.Errorf("%v: %v", args, err)
		}
	}
}

// world and airports are recognised whatever their position or case; a
// repeated code counts once.
func TestFetchTargets(t *testing.T) {
	world, airports, codes, err := fetchTargets([]string{"hr", "Airports", "world", "CH", "HR"})
	if err != nil || !world || !airports || !reflect.DeepEqual(codes, []string{"HR", "CH"}) {
		t.Errorf("got world=%t airports=%t codes=%v err=%v", world, airports, codes, err)
	}
	if _, airports, codes, err := fetchTargets([]string{"airports"}); err != nil || !airports || len(codes) != 0 {
		t.Errorf("airports alone: got airports=%t codes=%v err=%v", airports, codes, err)
	}
	for name, args := range map[string][]string{
		"nothing":     nil,
		"alpha-3":     {"HRV"},
		"digits":      {"12"},
		"digit after": {"HR", "H1"},
	} {
		if _, _, _, err := fetchTargets(args); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// Coordinates outside the globe, NaN included, and non-positive worker or
// negative limit values are errors, not silent defaults.
func TestInvalidInputRejected(t *testing.T) {
	a := quiet(t.TempDir())
	for _, args := range [][]string{{"NaN", "0"}, {"0", "Inf"}, {"90.1", "0"}, {"0", "-180.1"}} {
		if err := a.call(append([]string{"lookup"}, args...)...); !isUsage(err) || !strings.Contains(err.Error(), "±90") {
			t.Errorf("lookup %v: %v", args, err)
		}
	}
	if err := a.call("run", "-limit", "-1"); !isUsage(err) || !strings.Contains(err.Error(), "-limit") {
		t.Errorf("limit -1: %v", err)
	}
	t.Setenv("DB_USERNAME", "")
	for _, args := range [][]string{{"fetch", "-workers", "0", "world"}, {"lookup", "-workers", "0", "1", "-1"}, {"run", "-workers", "0"}} {
		if err := a.call(args...); !isUsage(err) || !strings.Contains(err.Error(), "-workers") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

// status reads the directory given and names the profile source.
func TestStatusOptions(t *testing.T) {
	dir := t.TempDir()
	w, err := cache.NewWriter(overture.WorldPath(dir), cache.Header{Kind: "world", Release: "2026-01-01.0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DB_USERNAME", "")
	var out strings.Builder
	a := quiet("data")
	a.out = &out
	err = a.call("status", "-data", dir)
	if err == nil || !strings.Contains(err.Error(), "DB_USERNAME") || !strings.Contains(out.String(), "world.geo          release 2026-01-01.0 rows 0") || !strings.Contains(out.String(), "\nprofiles bundled\n  default  [locality borough localadmin macrohood neighborhood microhood] smallest-area state=[region macroregion county macrocounty dependency] fallback=0.01") || !strings.Contains(out.String(), "\n  DE       [county") {
		t.Errorf("status -data: %v; printed %q", err, out.String())
	}
}

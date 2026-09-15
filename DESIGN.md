# Design

immich-placenames is a CLI built as one static Go binary. The `run` command makes one pass; scheduling is external. Overture is the only implemented provider.

## Boundary selection

`world.geo` includes countries and dependency territories. They can overlap: Gibraltar wins over Spain's territorial polygon on bounding-box size; both are territorial.

The resolver finds a country in `world.geo`, then reads that country's divisions. It checks bounding boxes before testing geometry, using even-odd containment with an edge tolerance of 0.00015 degrees. Each polygon has its own box, so lookups can skip distant parts of countries that cross the antimeridian.

Distance checks use the edge tolerance for countries and airports. Divisions use the larger of `fallbackDistance` and the edge tolerance. Candidates beyond these limits cannot affect the result. `lookup` uses exact distances for its diagnostic report.

Decoding allocates the polygon boxes once. Geometry lookups reuse them to skip boundary scans without allocating.

Exact containment and matches within the edge tolerance use the same ranking:

| Result | Ranking |
|---|---|
| Country | Territorial status, smaller bounding-box area, ID |
| City or state | Preferred subtype, lower administrative level (unknown last), territorial status, bounding-box area, ID |
| Airport | Class, distance to bounding-box centre, ID |

The profile sets the city's area tie-break; states always prefer the smaller area. See [Profiles](HOWTO.md#profiles) for settings and examples.

If no preferred subtype contains the point, the resolver tries nearby boundaries within `fallbackDistance` (default 0.01 degrees). Their bounding boxes must still contain the point. They rank by subtype preference, edge distance, then ID.

When airport matching is enabled, a containing airport replaces the city. Nearby airports do not qualify. An empty city falls back to the state, then the country.

## Database writes

The writer updates `asset_exif.city`, `state` and `country` directly in PostgreSQL, joining to `asset` through `assetId`.

A run selects pages in `(createdAt, id)` order, up to the greatest eligible key at startup. It resolves each page before opening a transaction, keeping downloads and geometry work outside the transaction.

Each update checks that the asset is undeleted and its coordinates are unchanged. City and country must still be null unless `-all` is set. Updates that fail these checks are skipped. `-lock` adds the three name fields to `lockedProperties`.

The default page size is 1000 assets; `-page-size 0` uses one transaction. Earlier commits survive failures or cancellation. No cursor is saved: ordinary reruns select the remaining unnamed assets.

Resolution errors skip affected assets and produce exit 1 after the pass. Database errors stop the pass. If a commit cannot be confirmed, the tool reports that uncertainty rather than claiming a rollback.

A dry run keeps all proposed names in memory to sort its CSV. Smaller pages won't bound that memory use.

## Fetching and storage

The fetcher reads raw Parquet values by leaf column and resets name fields between rows to prevent names leaking from one record to the next.

Each `.geo` file contains WKB geometry followed by a gob header and index. The header marks dependency support. Empty fetches fail and preserve existing files. Writers sync a temporary file before renaming it. Readers validate caches on open; invalid geometry fails the lookup.

HTTP requests time out after two minutes, including body reads. Fetch workers read row groups in parallel; assets are resolved sequentially. See [DATA.md](DATA.md) for transfer and memory measurements.

## Code layout

| Package | Responsibility |
|---|---|
| `cmd/immich-placenames` | CLI, diagnostics and run loop |
| `internal/geocode` | Provider interface, result types and final city fallback |
| `internal/geo` | Geometry decoding, containment and edge distance |
| `internal/cache` | Cache format and publication |
| `internal/overture/fetch` | Overture listing and Parquet reads |
| `internal/overture` | Boundary selection, profiles and explanations |
| `internal/immich` | Database selection, conditional writes and reset |

## Tests

`go test ./...` runs the unit tests. Real-cache tests use the public reference points and optional private fixtures described in [testdata/README.md](testdata/README.md).

Database tests use disposable schemas or session-local temporary tables. Use a disposable PostgreSQL database with permission to create those fixtures. Set `IMMICH_DSN`, or export the usual `DB_*` variables and set `IMMICH_TEST_DB=1`, then run:

```sh
go test ./internal/immich ./cmd/immich-placenames
```

They check selection bounds, overlapping writers, page rollback, locks, reset and cancellation. `IMMICH_BENCH=1` additionally enables the 10,000-asset transaction timing test.

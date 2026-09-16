# Design

immich-placenames is a CLI built as one static Go binary. The `run` command makes one pass; scheduling is external. Overture is the only implemented provider.

Names come from Overture's `divisions` theme, with airport polygons from `base`. Census boundaries, protected areas and national parks are outside the planned scope. Supporting them would require surveying Overture's available subtypes and adding a cache kind for sources such as `base` land use.

## Boundary selection

`world.geo` includes countries and dependency territories. They can overlap: Gibraltar wins over Spain's territorial polygon on bounding-box size; both are territorial.

The resolver finds a country in `world.geo`, then reads that country's divisions. It checks bounding boxes before testing geometry, using even-odd containment with an edge tolerance of 0.00015 degrees. Each polygon has its own box, so lookups can skip distant parts of countries that cross the antimeridian.

Distance checks use the edge tolerance for countries and airports. Divisions use the larger of `fallbackDistance` and the edge tolerance. Candidates beyond these limits cannot affect the result. `lookup` uses exact distances for its diagnostic report.

For division points, the resolver first searches a square covering the spherical circle of radius `pointDistance`. The search wraps longitude at the antimeridian and includes all longitudes if the circle reaches a pole. It then checks each label's great-circle distance against the metre limit. `lookup` searches a window based on twice that limit to show nearby rejected labels; the selection limit stays the same.

The resolver keeps spatial indexes in memory to avoid scanning every cached boundary for each photo. It builds them on first use and reuses them for later photos in the same run. Within each boundary, lookups also skip groups of segments that cannot affect containment or distance. Lookups still check the candidate boundaries before choosing a name.

Each cache file's grid stores at most 64 MiB of row references. Files with heavily overlapping boundaries use a coarser grid to limit memory use. If the index cannot fit within its limits, lookups scan the file's rows instead.

Matches within the edge tolerance rank the same as exact containment. Candidates rank as follows:

| Result | Ranking |
|---|---|
| Country | Territorial status, smaller bounding-box area, ID |
| City or state | Preferred subtype, lower administrative level (unknown last), territorial status, bounding-box area, ID |
| Division point | Preferred subtype, distance, ID |
| Airport | Class, distance to bounding-box centre, ID |

The profile sets the city's area tie-break; states always prefer the smaller area. See [Profiles](HOWTO.md#profiles) for settings and examples.

The resolver skips candidates whose names match a regular expression in `rejectNamePatterns`. Matching ignores case by default. This applies to divisions, division points and airports, but not to the country name from `world.geo`.

Patterns apply to all roles by default. An object entry can limit a pattern to `city`, `state` or `country`; the country role applies only to `countryFrom` divisions. Rejection happens during selection, so a candidate rejected for the city can still supply the state.

With `countryFrom`, the resolver uses a containing division for the country name and excludes it from state and city selection. The country code from `world.geo` still selects the divisions cache and profile.

If no preferred subtype contains the point, the resolver tries nearby boundaries within `fallbackDistance` (default 0.01 degrees). Their bounding boxes must still contain the point. They rank by subtype preference, edge distance, then ID.

If neither step finds a city and `pointFallback` is enabled, the resolver searches the country's division points within `pointDistance` (default 500 metres). Labels rank by subtype preference, distance, then ID. The resolver selects the containing division of a state subtype with the smallest bounding box and rejects labels outside its geometry. If no such division contains the query point, the resolver skips this check.

When airport matching is enabled, a containing airport replaces the city. Nearby airports do not qualify.

The resolver applies `cityOverrides` after airport matching. Entries match the resolved city name case-insensitively, with an optional state filter.

The resolver fills an empty city last, using `cityFallback` in order: state, then country by default. A division subtype, such as `county`, uses the same selection rules as the state: a containing division, then the nearest within `fallbackDistance`. Division subtypes use the city role for name rejection. The state and country sources copy the resolved name without checking patterns again. An empty list leaves the city empty. Fallback names bypass city overrides and leave the state and country unchanged.

The resolver selects fallback divisions after name rejection but before assigning country, state or city roles, so a division can supply more than one field. It applies the fallback only after the earlier matching steps and city overrides.

## Database writes

The writer updates `asset_exif.city`, `state` and `country` directly in PostgreSQL, joining to `asset` through `assetId`. It stores an empty city or state as NULL.

A run selects pages in `(createdAt, id)` order, up to the greatest eligible key at startup. It resolves each page before opening a transaction, keeping downloads and geometry work outside the transaction.

Each update checks that the asset is undeleted and its coordinates are unchanged. City and country must still be null unless `-all` is set. Updates that fail these checks are skipped.

`-lock`, on by default, adds `city`, `state` and `country` to `lockedProperties`. Immich's metadata extraction skips columns in that array. Otherwise, it replaces the names with its own when reverse geocoding is enabled and clears them when it is disabled.

Immich's list of lockable fields contains seven other fields. These three work because `lockedProperties` is an unconstrained `varchar[]` and Immich checks array membership. A constraint restricting locks to Immich's list would reject them. Editing an asset's description, date, location or rating triggers a sidecar write, which clears its locks.

`run` reads the reverse geocoding setting from `system_metadata`. It warns if the setting is enabled or absent; Immich's default is enabled. A disabled value is logged at info level. With reverse geocoding enabled, Immich names assets on import and the default selection skips them.

Both `run` and `status` report the stored setting, or Immich's default if none is stored. Neither can determine the running server's setting. `IMMICH_CONFIG_FILE` overrides the database without deleting existing rows, so stored values may be stale.

The default page size is 1000 assets; `-page-size 0` uses one transaction. Earlier commits survive failures or cancellation. No cursor is saved: ordinary reruns select the remaining unnamed assets.

Resolution errors skip affected assets and produce exit 1 after the pass. Database errors stop the pass. If a commit cannot be confirmed, the tool reports that uncertainty rather than claiming a rollback.

A dry run keeps all proposed names in memory to sort its CSV. Smaller pages won't bound that memory use.

## Fetching and storage

The fetcher reads raw Parquet values by leaf column and resets name fields between rows to prevent names leaking from one record to the next.

Each `.geo` file contains WKB geometry followed by a gob header and index. Each division-point row stores a point geometry and its zero-area bounding box. The header marks dependency support. Empty fetches fail and preserve existing files. Writers sync a temporary file before renaming it. Readers validate caches on open; invalid geometry fails the lookup.

HTTP requests time out after two minutes, including body reads. Fetch workers read row groups in parallel; assets are resolved sequentially. See [DATA.md](DATA.md) for transfer and memory measurements.

## Code layout

| Package | Responsibility |
|---|---|
| `cmd/immich-placenames` | CLI, diagnostics and run loop |
| `internal/geocode` | Provider interface, result types and the city fallback |
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

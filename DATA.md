# Geographic data

immich-placenames downloads geographic boundaries from Overture Maps and caches them on disk. The binary includes the profile settings, but you need to download the geographic data before the first lookup.

| Cache | Overture source | Used for |
|---|---|---|
| `world.geo` | Divisions / division_area, subtype country | Country and its two-letter code |
| `divisions/CC.geo` | Divisions / division_area, filtered by country | State and city |
| `airports.geo` | Base / infrastructure, subtype airport | Airport name in place of the city |

World and country caches include boundaries on land and at sea. These can provide names for photos taken off the coast, outside the land boundary.

The airport cache covers the whole world. Overture's infrastructure records have no country field, and filtering by a country's bounding box can still cover most of the globe. We keep one airport cache to avoid storing the same airports in several country caches.

Use [profiles](HOWTO.md#profiles) to choose boundary types and the order of preferred languages. The names and translations come from Overture; changing a profile won't add a missing translation.

## Download costs

We measured these fetches with release `2026-08-19.0` and four fetch workers. World and US downloads ran on a workstation; the airport download ran on a VM. Your timings will depend on hardware and network speed.

| Fetch | Rows kept | Cache size | Data read | Time | Peak RSS |
|---|---|---|---|---|---|
| World | 378 | 164 MB | 1,653 MB | 1m47s | 588 MB |
| Airports | 46,064 | 24 MB | 13,371 MB | 22m24s | 541 MB |
| US divisions | 59,770 | 236 MB | 356 MB | 26s | 288 MB |

The download can be much larger than the cache. Overture stores the data in Parquet files, which the tool reads in chunks using HTTP range requests. It reads metadata and selected row groups, then keeps only the matching records. Airport records are scattered through the infrastructure dataset, so building a 24 MB cache required about 13.4 GB of reads in this run.

`lookup` and `run` download missing caches, even during a dry run. Later lookups reuse them. Downloads come from Overture's public S3 bucket and need no API key.

## Releases and cache files

Each cache records which Overture release it came from. By default, `fetch` uses the latest release in the [Overture catalog](https://stac.overturemaps.org/catalog.json). Use `-release` to choose one explicitly. When `lookup` or `run` downloads missing country or airport data, it uses the release recorded in `world.geo`.

Caches stay on disk between runs. A new Overture release won't trigger an automatic refresh. Use `status` to check cache releases; `run` warns if it opens caches from different releases. When refreshing, use the same release for all caches in the data directory.

See the [operating guide](HOWTO.md#caches-and-refreshes) for downloading data ahead of time, refreshing caches and handling incompatible cache formats. The real-data tests require a [specific release and profile](testdata/README.md).

## Privacy and network access

The tool reads coordinates from Immich's database and resolves names locally. It needs no access to photo files and does not send individual photo coordinates to a remote geocoding API.

The tool contacts Overture's catalog and public S3 storage to download data. Those hosts can see your server's public IP address and which data you request. With the required caches in place, lookups need no further downloads.

Dry-run CSVs and debug logs include asset IDs and place names. Database exports from the HOWTO also include coordinates. Review these files before sharing them in a public issue.

## Attribution and licenses

Overture lists both the Divisions and Base themes under the Open Database License (ODbL). The underlying sources are listed in [NOTICE](NOTICE) and [Overture's attribution page](https://docs.overturemaps.org/attribution/).

© OpenStreetMap contributors, Overture Maps Foundation.

The software and bundled profile catalog use [AGPL-3.0](LICENSE). The catalog and resolver rule tests came from [Immich ReverseGeo](https://github.com/immich-reversegeo/immich-reversegeo).

The tool builds polygon caches on your machine; Git ignores them. The test fixtures include expected place names derived from Overture, with coordinates chosen for those tests. If you distribute caches or derived data, check the [ODbL terms](https://opendatacommons.org/licenses/odbl/1-0/).

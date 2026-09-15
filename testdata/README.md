# Overture regression fixtures

These tests use Overture release `2026-08-19.0` and this directory's [profiles.json](profiles.json). They ignore any profile in the cache directory.

[samples.csv](samples.csv) has 19 reference points across AT, CH, DE, HR, ID, IN and US; [sea.csv](sea.csv) has two offshore points. We chose grid points and named sites for the tests, without copying photo coordinates. Case IDs label locations and have no link to Immich asset IDs.

We checked the expected names against that release's boundaries and resolver results. The tests catch changes in those results; they don't independently verify geographic accuracy.

| Cases | What they check |
|---|---|
| Dubrovnik, Lapad, Komolac, Cavtat | HR municipality preference and boundary containment |
| Rafz and Jestetten | Swiss/German country selection on opposite sides of the border |
| Zürich and Dubrovnik airports | Names with airport matching enabled and disabled |
| Delhi and Faridabad | Local subtype first, county when no preferred local subtype contains |
| Vienna, Ubud, Miami Beach, Fort Lauderdale, Westwood | District and locality selection |
| Lapad and Cavtat water | Country/state at sea, nearest-city distance limit, state fallback with distance zero |
| Everglades and Death Valley | The bundled US county fallback where no locality reaches the point |
| Key West water | A nearby locality still outranks a containing county |
| Jestetten district names | Bundled German ranking, its association names and the two ways past them |
| Lapad language test | Croatian primary names and German translations |
| Dependency territories | Country selection across 105 polygons and 53 codes |

Generated dependency points use the resolver's edge tolerance. Thirteen named places use coordinates chosen by hand.

## Run

From the project root, download the caches first. The tests never download data:

```sh
go run ./cmd/immich-placenames fetch -data ./data -release 2026-08-19.0 world airports AT CH DE HR ID IN US
REVERSEGEO_DATA="$PWD/data" go test ./internal/overture -run 'TestOracle|TestSeaFallback' -count=1
```

You need `world.geo`, `airports.geo` and `divisions/{AT,CH,DE,HR,ID,IN,US}.geo`, all from the release above. The HR cache must include primary names and German translations. Git ignores the caches.

The benchmarks use the same caches and measure lookups after decoding the geometry:

```sh
REVERSEGEO_DATA="$PWD/data" go test ./internal/overture -run '^$' -bench 'Country|Resolve'
```

Dependency tests need only `world.geo`:

```sh
go run ./cmd/immich-placenames fetch -data ./data -release 2026-08-19.0 world
REVERSEGEO_DATA="$PWD/data" go test ./internal/overture -run TestOracleDependenc -count=1
```

Without `REVERSEGEO_DATA`, these tests skip. Once you set it, missing, corrupt or wrong-release caches cause failures. No Immich database is needed. Run the synthetic unit tests with `go test ./...`.

## Optional library fixtures

To test your own coordinates and expected names, put them in a separate directory. The test still uses the release and profile above:

```sh
REVERSEGEO_DATA=/absolute/cache/path \
REVERSEGEO_PRIVATE_FIXTURES=/absolute/private/testdata/path \
go test ./internal/overture -run '^TestPrivateOracle$' -count=1
```

Provide both `samples.csv` and `sea.csv`, each with a header and at least one data row. Use `caseId,lat,lon,city,state,country`; the first column can also be named `assetId`. IDs must be non-empty and unique within each file. Keep personal coordinates and asset IDs outside this repository.

## Provenance

Expected place names come from Overture release `2026-08-19.0`. Attribution: © OpenStreetMap contributors, Overture Maps Foundation. See [DATA.md](../DATA.md) and [NOTICE](../NOTICE) for sources and licensing.

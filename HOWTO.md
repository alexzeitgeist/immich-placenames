# Using immich-placenames

These examples run from the source directory. Build with Go 1.26 or later:

```sh
CGO_ENABLED=0 go build -buildvcs=false ./cmd/immich-placenames
```

For Docker, complete the [Compose setup](deploy/README.md), then replace `./immich-placenames` with:

```sh
sudo docker compose --project-directory /opt/immich-placenames \
  -f /opt/immich-placenames/compose.yml \
  run --rm --no-deps immich-placenames
```

Add `-data /data` to `fetch`, `lookup`, `run` and `status`. Profile and ID files must be inside the mounted data directory; use their container paths, such as `/data/try.json`.

## Database connection

`run`, `status` and `reset` need these environment variables:

| Variable | Binary default |
|---|---|
| `DB_HOST` | `database` |
| `DB_PORT` | `5432` |
| `DB_USERNAME` | Required |
| `DB_PASSWORD` | Required |
| `DB_DATABASE_NAME` | Required |

Export your Immich database credentials before running the binary; it does not read `.env`. Compose reads `.env` and passes the values to the container, defaulting to user `postgres` and database `immich`.

`database` normally resolves inside Immich's Docker network. When running outside Docker, set a reachable database address. The Compose setup uses the existing network without publishing a database port.

`fetch` and `lookup` need no database. `status` prints cache and profile information before connecting, so a database error may follow that output.

## Lookups

```sh
./immich-placenames lookup 47.46 8.55
./immich-placenames lookup -airports=false 47.46 8.55
./immich-placenames lookup -airports=false -fallback-distance 0 47.46 8.55
./immich-placenames lookup -points -point-distance 800 47.46 8.55
```

The first lookup uses your profile. The second disables airport naming; the third also disables nearest-division matching. The fourth enables [point fallback](#places-without-a-boundary) within 800 metres and downloads the country's point cache if needed. These flags apply only to that lookup. The resolver fills an empty city using [`cityFallback`](#when-nothing-names-the-city).

The output lists polygons whose bounding boxes contain the point. `IN` means the polygon contains it too; `bbox` means it does not. Each line includes subtype, administrative level, territorial status (`T`), bounding-box area, name language and decision. Area is in units of 0.0001 square degrees; edge distance is in degrees.

`city`, `state` and `airport` mark selected candidates. `city, nearest` means the coastal fallback supplied the city. `contains, outranked` means another containing candidate won under the profile rules. Add `-json` for the same explanation as JSON.

A `points/CC.geo` block appears when point fallback runs. Label distances are in metres, with nearby labels marked `near`. `city, point` marks the selected label; `outside NAME` means the label lies outside the containing administrative area; `beyond the distance` means it exceeds `pointDistance`. The diagnostic search uses a square large enough to include labels within twice `pointDistance`, so it can also list more distant labels. Only labels within `pointDistance` can be selected. An `override:` line shows any city-name replacement, and a `city fallback:` line names the source that filled an empty city.

Put flags before coordinates. Negative coordinates need no `--`: `lookup -8.51 115.26`.

## Preview and write

```sh
./immich-placenames run -dry-run > dry-run.csv
./immich-placenames run -dry-run -all > all-names.csv
```

The first command selects assets with null city and country. The second includes already-named assets. Both require coordinates and exclude deleted assets.

The output contains the build revision, cache releases and profiles, then an `assetId,city,state,country` CSV and grouped summary. The `# names` line counts names by language; `# fallback` counts city fallbacks by source. A write run logs the same total as `city_fallback`.

`-limit 50` selects at most 50 assets in creation-time and ID order. Failures and points with no matching country count toward the limit.

To write names, repeat the command without `-dry-run`, keeping the same profile and selection flags. This reads the database again; assets may have changed since the preview.

Writes commit in pages of 1000 selected assets. Use `-page-size N` to change this, or `-page-size 0` for one transaction. Before writing, the tool checks for deletion, changed coordinates and, unless using `-all`, city and country still being null. See [database writes](DESIGN.md#database-writes) for transaction details.

If a run stops, earlier commits remain. Rerun normally to pick up unnamed assets; `-all` can overwrite names again. If the tool cannot confirm a commit, check the database before assuming it rolled back.

`-lock` protects the three name fields from metadata extraction by adding them to Immich's `lockedProperties`. It is off by default and does not block `run -all`.

Add `-v` for debug logs:

```sh
./immich-placenames run -v -dry-run > dry-run.csv
```

Logs go to stderr. `resolved` is a proposed update. During write runs, `page committed` confirms a committed page and includes transaction timings.

### Compare with stored names

With the database variables exported and `psql` installed, export the current names as CSV:

```sh
PGPASSWORD="$DB_PASSWORD" psql -X \
  -h "${DB_HOST:-database}" -p "${DB_PORT:-5432}" \
  -U "$DB_USERNAME" -d "$DB_DATABASE_NAME" \
  -c 'COPY (
    SELECT a.id, e.latitude, e.longitude, e.city, e.state, e.country
    FROM asset a JOIN asset_exif e ON e."assetId" = a.id
    WHERE a."deletedAt" IS NULL
      AND e.latitude IS NOT NULL AND e.longitude IS NOT NULL
    ORDER BY a.id
  ) TO STDOUT WITH (FORMAT csv)' > database-names.csv
./immich-placenames run -dry-run -all > proposed-names.csv
python3 deploy/compare.py database-names.csv proposed-names.csv
```

For a database reachable only inside Docker, run the same `COPY` query through `psql` in its container. Keep CSV format so commas in names are quoted.

The script counts equal and changed names for matching asset IDs and lists each rename. Check the resolved count and logs too: unmatched or failed assets are not counted as differences.

## Profiles

Start with `lookup`. Check whether the boundary you want appears and contains the point. Profiles cannot add missing boundaries or translations. Try [`pointFallback`](#places-without-a-boundary) if no boundary supplies a city name. If you want the municipality instead of an airport name, try `-airports=false` first.

The tool loads `profiles.json` from the data directory, using bundled settings if the file is absent. An explicit `-profiles FILE` must exist.

Fields merge in this order: bundled default → bundled country → user default → user country. Your global defaults can override the bundled DE/FI/FR/US rules. Missing fields, nulls, empty lists and blank strings inherit; `false` and zero override. For `rejectNamePrefixes` and `cityFallback`, an empty list clears the inherited value instead.

For example, to prefer municipality boundaries in Croatia:

```json
{
  "countryOverrides": {
    "HR": {
      "preferredSubtypes": ["county", "locality", "borough", "localadmin", "macrohood", "neighborhood", "microhood"],
      "tieBreakMode": "largest-area"
    }
  }
}
```

This prefers `county`, then the largest bounding box when other city rankings tie.

| Field | Default | Effect |
|---|---|---|
| `preferredSubtypes` | `locality, borough, localadmin, macrohood, neighborhood, microhood` | City boundary types, in preference order |
| `stateSubtypes` | `region, macroregion, county, macrocounty, dependency` | State boundary types, in preference order |
| `tieBreakMode` | `smallest-area` | Prefer the smallest or largest bounding box when other city rankings tie |
| `fallbackDistance` | `0.01` degrees | Bound for nearest-division matching; zero disables it |
| `airports` | `true` | Let a containing airport replace the city |
| `language` | `en` | One name language or a list, tried in order |
| `pointFallback` | `false` | Use nearby division points when no area supplies a city name |
| `pointDistance` | `500` metres | Bound for division-point matching; zero disables it |
| `cityOverrides` | none | Replace resolved city names |
| `rejectNamePrefixes` | none | Skip names starting with a listed prefix |
| `countryFrom` | none | Division subtype whose name replaces the country |
| `cityFallback` | `state, country` | Sources for an empty city: state, country or a division subtype |

Both subtype lists accept `country`, `dependency`, `region`, `macroregion`, `county`, `macrocounty`, `localadmin`, `locality`, `borough`, `macrohood`, `neighborhood` and `microhood`.

`countryFrom` accepts the same subtype names, and `cityFallback` accepts them alongside `state` and `country`. Country keys use two-letter codes such as `HR`. Unknown fields, invalid country keys, unknown subtypes, tie-break modes or fallback sources, negative distances, city overrides with an empty `from`, empty reject prefixes and malformed language codes fail when the file loads.

Fallback distance is measured in degrees, not metres. Candidates' bounding boxes must still contain the point, and subtype preference ranks before distance. `pointDistance` is measured in metres. See [boundary selection](DESIGN.md#boundary-selection).

### Places without a boundary

Overture stores place labels as division points, including for places without boundaries. Without a matching city boundary, a photo in a Stockholm suburb can get the county name. Enable point fallback to use nearby labels:

```json
{
  "countryOverrides": {
    "SE": {
      "pointFallback": true,
      "pointDistance": 500
    }
  }
}
```

Point fallback is off by default. When enabled, it downloads a [point cache](DATA.md#geographic-data) if needed. Labels rank by subtype preference first. With the default order, a `locality` within the distance limit takes priority over a closer `neighborhood`. Keep the distance small unless a lookup shows that a wider search gives better names.

The resolver checks areas with subtypes listed in `stateSubtypes`. Of those containing the photo's coordinates, it uses the one with the smallest bounding box. It rejects labels outside that area's boundary and lists them as `outside NAME`. If no such area contains the coordinates, only the subtype and distance limits apply.

Try it on a coordinate first:

```sh
./immich-placenames lookup -points -point-distance 2000 59.241061 18.101586
```

### When nothing names the city

If no boundary or label supplies a city, the default fallback copies the state, or the country if the state is empty. With this fallback, a photo in the Everglades gets `Florida, Florida, United States`.

The bundled US profile tries a county first:

```json
{
  "countryOverrides": {
    "US": {
      "cityFallback": ["county", "state", "country"]
    }
  }
}
```

This gives `Monroe County, Florida, United States` at the Everglades example below. Other examples include `Inyo County, California, United States` in Death Valley and `San Francisco, California, United States` in San Francisco Bay.

You can configure division subtypes for other countries too. To leave the city empty, set `"cityFallback": []`. The writer stores an empty city as NULL.

The resolver tries `state`, `country` and division subtypes in the listed order. For a subtype, it uses the same ranking as state selection: a containing division first, then the nearest within `fallbackDistance`. It skips names matched by `rejectNamePrefixes` and tries the next source if no candidate remains.

The resolver applies `cityFallback` after nearest-division matching, point fallback, airports and city overrides, and only if the city is empty. A county in this list leaves a nearby locality's name intact. Putting county in `preferredSubtypes` gives a containing county priority over nearby localities. The fallback leaves the state and country unchanged.

Try it on a coordinate first:

```sh
./immich-placenames lookup -profiles data/try.json 25.4 -80.9
```

The lookup prints the source on a `city fallback:` line and marks the selected division `city fallback`. `run -dry-run` counts the filled cities by source on its `# fallback` line.

### Renaming a city

Where the resolver picks a name you do not want, rewrite it:

```json
{
  "countryOverrides": {
    "SE": {
      "cityOverrides": [
        {"from": "Mörtvik", "to": "Skogås", "state": "Stockholm County"}
      ]
    }
  }
}
```

`from` matches the resolved city name, including airport names. Matching ignores case. The first matching entry wins. Add `state` to distinguish places with the same name. An empty `to` clears the city before the [city fallback](#when-nothing-names-the-city) runs.

If `cityFallback` includes the subtype that supplied the cleared name, it can restore that name. Remove the subtype from the chain, or use [`rejectNamePrefixes`](#skipping-names) to reject the name during selection, including fallback selection.

Overrides apply only to resolved city names. They leave state and country fields unchanged, and do not apply to a name the city fallback copied. `lookup` reports each rewrite on an `override:` line, and `status` counts the entries in each profile.

### Skipping names

The German profile prefers counties to keep city names such as Stuttgart or Munich. To avoid district and administrative association names for rural photos, it rejects six prefixes:

```json
"rejectNamePrefixes": ["Landkreis ", "Kreis ", "GVV ", "VVG ", "Samtgemeinde ", "Verwaltungsgemeinschaft "]
```

The first two prefixes exclude districts; the other four exclude administrative associations. Overture classifies `GVV Jestetten`, `VVG der Stadt Konstanz` and `Samtgemeinde Bersenbrück` as `locality`, just like their member municipalities. The German profile prefers the largest bounding box when other rankings tie. Rejecting only district names therefore gives `GVV Jestetten` instead of `Jestetten`.

Cities that are counties of their own, such as Stuttgart, have none of these prefixes and keep their names.

These prefixes were checked against Overture release `2026-08-19.0`; they may not cover every association name. The list excludes `Amt ` because municipalities such as Amt Neuhaus use it.

To name German photos after their district again, clear the list:

```json
{
  "countryOverrides": {
    "DE": {
      "rejectNamePrefixes": []
    }
  }
}
```

With `smallest-area`, the resolver can select a village within the municipality:

| Setting | At Jestetten | At Litzelstetten near Konstanz |
|---|---|---|
| Bundled | Jestetten | Constance |
| `"tieBreakMode": "smallest-area"` | Jestetten | Litzelstetten |

Choose municipality or village names to suit your library. Both rows use English names. With `"language": "de"`, `Constance` becomes `Konstanz`; `Litzelstetten` stays the same.

No other country rejects prefixes by default. If you set prefixes in `defaultProfile`, use `"rejectNamePrefixes": []` in a country override to clear the inherited list.

Matching ignores case. `ß` matches `ẞ`, but not `SS`. Write prefixes in your chosen name language. The trailing space in `Kreis ` rejects `Kreis Steinfurt` but keeps the municipality `Kreischa`.

Prefixes apply to city, state and airport names, including nearby place names used as fallbacks. They also apply to country names chosen with `countryFrom`, but leave the default country name unchanged. `lookup` marks rejected candidates with `name rejected, prefix "Landkreis "`.

### Naming the country after a region

To use Scotland as the country name, set `countryFrom` to `region` for GB:

```json
{
  "countryOverrides": {
    "GB": {
      "countryFrom": "region"
    }
  }
}
```

An Edinburgh photo changes from `Old Town, Scotland, United Kingdom` to `Old Town, City of Edinburgh, Scotland`. Scotland is used only for the country field, leaving City of Edinburgh as the state.

Keep this setting under `GB`, even when the country field reads Scotland. If no matching region contains the photo, or its name is missing or rejected, the original country name stays. `lookup` shows the change on a `country:` line and marks the selected region `country`.

### Languages

To prefer German names in every country:

```json
{
  "defaultProfile": {
    "language": "de"
  }
}
```

Overture supplies the translations. `primary` means the local name. Each name uses the first available language in the chain, then the row ID if none exists.

| Setting | Tried in order |
|---|---|
| `"en"` | English, primary |
| `"de"` | German, English, primary |
| `["de", "primary"]` | German, primary, English |
| `"primary"` | Primary, English |

For German names only in AT, CH, DE and HR, set `"language": "de"` in those country overrides and leave the default as English. You can combine this with the HR county-first settings above.

For India, append `county` to the default `preferredSubtypes` list in an `IN` override and keep `"tieBreakMode": "smallest-area"`. This considers counties after the other city types.

A profile edit is usually enough to change languages. Check older caches with `status`: `en`, `en+primary` and `en+primary+common(...)` show which name fields they contain. Refetch the same release if needed.

Save a trial profile as `data/try.json`, then inspect it:

```sh
./immich-placenames lookup -profiles data/try.json 42.65 18.07
./immich-placenames run -profiles data/try.json -dry-run -all > trial.csv
```

After review, save it as `data/profiles.json`. Existing names stay unchanged until a reset or `run -all`.

## Reset names

`reset` clears city, state, country and their locks. Preview the selection first:

```sh
./immich-placenames reset -dry-run -country Croatia
```

Remove `-dry-run` to clear those rows. The next normal run names them under the current profile.

Choose exactly one selector: `-all`, `-ids FILE`, `-city VALUE`, `-state VALUE` or `-country VALUE`. Names match the stored value exactly, so use `Croatia`, not `HR`. An ID file contains one asset ID per line and accepts `#` comments.

Reset includes trashed assets. Normal runs exclude them until restored. To replace all existing names without clearing them first, use `run -all`.

## Caches and refreshes

`lookup` and `run`, including dry runs, fetch missing caches at the release recorded in `world.geo`. They and country-division fetches rebuild world caches lacking dependency territories (about 1.8 GB). Failed downloads keep the old file; the next invocation that needs it retries.

To prefetch a known release:

```sh
./immich-placenames fetch -release 2026-08-19.0 world airports HR CH
./immich-placenames fetch -release 2026-08-19.0 -points HR CH
./immich-placenames fetch -release 2026-08-19.0 -areas=false -points HR CH
```

Replace HR/CH with the countries you need. The second command fetches areas and points; the third adds points to existing caches without downloading the areas again. Without `-release`, `fetch` selects the latest release from Overture's catalog, which may differ from your existing caches.

`-release` applies to the requested caches. Automatic world migration keeps its recorded release; include `world` in the fetch targets to change it.

To refresh, pause the timer and fetch world, airports and all required countries at the same release. Include `-points` for countries where you use point fallback. Check `status` and a dry run before resuming. Use the same procedure after a cache-format change; invalid caches are treated as missing and rebuilt.

Fetches use four workers by default. Try `-workers 2` if memory is tight. See [measured costs](DATA.md#download-costs); the cache's final size is much smaller than the transfer.

## Status and failures

`status` lists each cache's release, row count, size, fetch time and available name languages, followed by effective profiles and the pending asset count. It does not fetch missing caches. Profile lines show the point-fallback setting, such as `points=true@500m`, and the number of city overrides as `cityOverrides=N` when configured. A city fallback other than the default appears as `cityFallback=[country]` or `cityFallback=[]`.

Exit codes are 0 for success, 2 for usage errors and 1 for operational failures. Exit 1 can follow successful writes to earlier pages.

| Message | What to do |
|---|---|
| `unknown command "-data"` | Put the command first, then flags, then arguments |
| `flag provided but not defined` | Check `COMMAND -h`; flags differ by command |
| `profiles: open ...` | Check the path; containers see the mounted file under `/data` |
| `DB_USERNAME, DB_PASSWORD and DB_DATABASE_NAME must be set` | Supply the database environment |
| `mixed cache releases` | Refetch the required caches with the same `-release` |
| `cache invalid, treated as absent` | Refetch the damaged or incompatible cache |
| `cache has no ..., falling back; refetch it` | Refetch for the missing name languages |
| `no country` | Inspect the coordinates; points outside all country and dependency polygons remain unnamed |
| `refetch failed, keeping the country polygons ...` | Retry the command or fetch `world` again |
| `changed meanwhile` | An asset changed before its write; a later run may select it again |
| `N assets failed` | Check the per-asset errors; other assets may have been written |
| `stopped after N confirmed writes in P committed pages` | Earlier commits remain; inspect the failure before rerunning |
| `dry-run stopped` | Check selection, cancellation or output errors; no database writes were attempted |

## Reporting wrong names

To report a wrong name, include:

- Tool version (`version`) and cache releases (`status`).
- Relevant profile settings and any lookup flags.
- A coordinate you are comfortable publishing, with the actual and expected names.
- The output from `lookup` for that coordinate.

Remove database credentials and unrelated personal information from attachments. Do not attach a full library export. See [privacy and network access](DATA.md#privacy-and-network-access) for what the output can contain.

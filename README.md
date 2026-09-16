# immich-placenames

immich-placenames assigns city, state and country names to photos in Immich that already have GPS coordinates. It runs on your own server and uses geographic boundaries from Overture Maps to find which places contain each photo's location.

You can choose which kinds of places to use for names. That might mean a municipality in Croatia or a district in Vienna, depending on the available boundaries and your settings.

## What you can do

- Choose the language for place names, with different settings per country. English is the default.
- Use airport names for photos taken within airport boundaries.
- Use nearby place labels where Overture has no boundary. Off by default.
- Replace resolved city names, with an optional state filter.
- Preview proposed changes before writing them, or look up individual coordinates without connecting to Immich.
- Fill missing names or reprocess existing ones, once or on a schedule.

## Before you start

The tool writes directly to Immich's PostgreSQL database. Back up the database and disable Immich's built-in reverse geocoding before using it to write names. Runs lock the names they write to stop Immich's metadata extraction from overwriting them. `reset` clears the locks. It has been tested with Immich v3.1.0.

The first run downloads geographic data and saves it locally for later runs. Allow for several gigabytes of network traffic; airport data alone required about 13.4 GB of downloads in our measurements. Airport naming is enabled by default and can be disabled. See [download and disk requirements](DATA.md).

## Install and use

Follow the [Docker Compose setup](deploy/README.md) to build the image, connect to your Immich database and preview changes. There is no published image yet. The setup also includes an optional hourly timer.

The tool runs from the command line and has no web interface. [HOWTO.md](HOWTO.md) covers building without Docker, lookups, language settings and replacing existing names.

For implementation details, see [DESIGN.md](DESIGN.md). Contributors can find the regression suite and optional integration checks in the [test instructions](testdata/README.md).

## Credit and license

This project began as a Go port of [Immich ReverseGeo](https://github.com/immich-reversegeo/immich-reversegeo). It carries that project's profile catalog and resolver rule tests under [AGPL-3.0](LICENSE).

Geographic data comes from Overture Maps. Attribution: © OpenStreetMap contributors, Overture Maps Foundation. See [DATA.md](DATA.md#attribution-and-licenses) and [NOTICE](NOTICE) for the data licenses and sources.

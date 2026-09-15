# Compose worker

Run immich-placenames as a separate Compose project on the same Docker host as Immich, connected to its existing database network. This setup has been tested with Immich v3.1.0.

Use `docker compose run --rm` for individual commands. Plain `docker compose up` runs one naming pass against the database and exits; use the timer below for hourly scheduling.

[Back up the database](https://docs.immich.app/administration/backup-and-restore/) and disable [Immich's built-in reverse geocoding](https://docs.immich.app/administration/system-settings/#reverse-geocoding-settings) before writing names.

Build from the project root:

```sh
docker build -t immich-placenames:local .
```

The container runs as UID/GID 10001:10001. Install the Compose files and create its cache directory on the Docker host:

```sh
sudo install -d -m 0755 /opt/immich-placenames
sudo install -m 0644 deploy/compose.yml /opt/immich-placenames/compose.yml
sudo install -m 0600 deploy/.env.example /opt/immich-placenames/.env
sudo install -d -o 10001 -g 10001 -m 0755 /opt/immich-placenames/data
```

Fill in the database credentials in `/opt/immich-placenames/.env`. Set `IMMICH_NETWORK` to your Immich network and `DB_HOST` to the database service alias on that network. The defaults are `immich_default` and `database`. Build or load the image on this host; there is no published image yet.

You can start with the bundled profiles. For different languages or boundary preferences, follow the [profile guide](../HOWTO.md#profiles) and save your settings in `/opt/immich-placenames/data/profiles.json`. The file must be readable by UID 10001.

If you reuse an existing cache, make sure UID/GID 10001:10001 can write to it, including its subdirectories. If you change `IMMICH_PLACENAMES_DATA` and use the systemd timer below, update `ReadWritePaths` in the service file too.

Check the configuration, download the data and preview the names:

```sh
cd /opt/immich-placenames
sudo docker compose config --quiet
sudo docker compose run --rm --no-deps immich-placenames fetch -data /data world airports HR CH
sudo docker compose run --rm --no-deps immich-placenames status -data /data
sudo docker compose run --rm --no-deps immich-placenames run -data /data -dry-run
```

Replace HR/CH with the countries in your library. With release `2026-08-19.0`, the airport download read about 13.4 GB and world boundaries about 1.8 GB. A dry run can download missing caches too. Later runs reuse them.

Review the dry-run output, then repeat the command without `-dry-run` to write the names. By default, the tool selects assets whose city and country are both null. Add `-all` to include already-named assets.

## Optional timer

After a successful manual run, install and enable the timer from the source checkout:

```sh
sudo install -m 0644 deploy/immich-placenames.service deploy/immich-placenames.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now immich-placenames.timer
systemctl list-timers immich-placenames.timer
sudo journalctl -u immich-placenames.service
```

The timer runs hourly, with a random delay of up to five minutes. It catches up with one run after downtime. Each run removes its container when finished and leaves the cache on disk.

The command uses `--no-deps`, so it won't start Immich or PostgreSQL. If the database is unavailable, the run fails without waiting for it to come up. The next scheduled run tries again.

The container has a 2 GB memory limit, read-only root filesystem, 64 MB writable `/tmp`, and no Linux capabilities. `Nice=10` only changes the Compose process's priority; it does not change the container workload's priority.

Stop scheduled writes with `sudo systemctl disable --now immich-placenames.timer`. If a run is already in progress, stop the service separately. Neither action undoes names already written.

## Update the image

Build from a clean checkout, using the commit hash as the image tag:

```sh
REVISION=$(git rev-parse HEAD)
docker build -t "immich-placenames:$REVISION" \
  --build-arg VCS_REF="$REVISION" --build-arg VERSION="$REVISION" .
```

`VCS_REF` sets the image revision label and the binary's version output. `VERSION` sets the image version label. Without build arguments, the binary reports `devel`; the revision label is `unknown` and the image version label is `dev`.

Stop the timer and wait for any active run to finish. Build or load the new image on the Docker host and keep the previous image for rollback. Set `IMMICH_PLACENAMES_IMAGE` in `.env` to the new tag. Check `version`, `status` and a full dry run before enabling the timer again.

If caches need rebuilding, [prefetch them](../HOWTO.md#caches-and-refreshes) before restarting the timer. Reverting an image does not undo names already written to Immich.

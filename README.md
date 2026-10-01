# Log Analytics

The official [NGINX UI](https://github.com/0xJacky/nginx-ui) plugin for
looking into your nginx access logs. It indexes the logs in the background, lets
you search them with structured filters, and shows how the traffic behaves: page
views and visitors over time, top pages, browsers, systems, devices and where
the visitors come from, on a map.

* Plugin id: `com.nginxui.log-analytics`
* Requires NGINX UI 2.7.0 or newer
* Plugin API version 1

## Features

* **Structured search** over the access logs, by time range, address, method,
  status code, path, browser, system, device and free text, with the totals of
  the whole match and the traffic they carry.
* **Traffic dashboard** for a log or a site: visitors and page views by hour and
  by day, the busiest pages, browsers, systems and devices, requests per second.
* **Visitor maps**: the world, the regions of each country and the busiest cities.
* **Log list columns**: for every log the state of its index, when it was last
  indexed, how many entries it holds and which time range they cover, plus
  rebuilding a log or all of them.
* **Traffic analysis entry** in the site editor and the site list.
* **Follows the logs you already have.** The plugin uses the access logs NGINX
  UI lists, including the rotated and compressed files next to them
  (`access.log.1`, `access.log.2.gz`, `access.log-20260101`). It indexes new
  lines as they arrive and starts over on a log that was rotated.
* **Nothing to run when it is off.** NGINX UI itself loads none of this. Without
  the plugin, or with it disabled, no memory goes to indexing or searching.

## Setting up

Install the plugin from the plugin page and enable it. Indexing starts on its
own: the logs are picked up within a minute and their state shows in the log
list. Turning indexing off is disabling the plugin, which stops the process and
frees everything it held.

For the visitor map by region, the plugin needs an IP location database. Open
the plugin settings and download it there. Without it the map still shows
countries, and provinces and cities stay empty.

Coming from a version of NGINX UI that had advanced indexing built in: the first
time the plugin starts it takes over the index and its records, the settings and
the downloaded location database, so nothing is indexed twice.

## Settings

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `incremental_index_interval` | number | 15 | Minutes between two checks of the logs for new lines. Zero or empty means 15. |
| `max_concurrent_index_tasks` | number | 0 | The most log files indexed at the same time. A lower number uses less memory. Zero derives it from the available CPUs, at most 2. |
| `index_custom_mmdb` | text | empty | Path of your own IP location database. A relative path is looked up in the `geolite` folder of the plugin data directory. Empty uses the downloaded database. |

The settings panel also downloads the IP location database and shows whether it
is installed.

## Capabilities

| Capability | Use |
| --- | --- |
| `http` (`listen: unix`) | The page talks to the plugin over the plugin HTTP route. Websockets carry the progress of indexing and the database download. |

The plugin declares no other capability and registers no cron entry: it keeps
its own schedule while it runs.

## Permissions and why

| Permission | Why it is needed |
| --- | --- |
| `log.files` | To ask NGINX UI which nginx log files it may read, and to be told when that set changes. The plugin reads those files, and only those and their rotated files, itself. |
| `network` | To download the IP location database from `cloud.nginxui.com`. Nothing else opens a connection. The download goes through the HTTP proxy configured in NGINX UI, when there is one. |
| `kv` | To remember where an index is kept when it could not be moved into the plugin data directory during the takeover from an older version. |

Access logs hold addresses, session parameters and user agents. They stay on the
machine: the index lives in the plugin data directory and no log content is sent
anywhere.

## Supported platforms

| OS | Architectures |
| --- | --- |
| Linux | amd64, arm64 |
| macOS | amd64, arm64 |
| Windows | amd64, arm64 |

The binaries are statically linked (`CGO_ENABLED=0`) and built with
`-trimpath -ldflags "-s -w"`. Each platform ships as its own package, see
[Packaging](#packaging).

On Windows the HTTP API listens on a loopback port instead of a socket. The
plugin SDK opens it and reports the port to NGINX UI during the handshake, so
nothing differs for you.

## Memory and responsiveness

This section describes how the plugin process behaves. It is a technical note,
nothing here needs configuring.

* **Nothing is opened at start.** The plugin process starts without opening an
  index shard or building a searcher. The state of every log (what the list
  shows) comes from a small database, so the list costs no shard either.
* **Warm up.** The views that will search, the list columns and the site entry
  send `POST /warm` as soon as they appear. The plugin then opens every shard in
  parallel in the background and answers `202` at once. By the time a person
  starts a search the shards are usually open. A search that arrives first opens
  them itself and waits for that, once, however many requests come in together.
* **Idle release.** After 10 minutes without a search, a statistics request, a
  warm up or an indexing run, the plugin closes the shards, drops the searcher
  and its result caches and returns the memory to the system. A request that is
  still running keeps them open. The next request opens them again.
* **Indexing rounds.** The periodic incremental round and every rebuild run as
  a round. A round starts the log parser and, when nobody is searching, closes
  the shards it opened as soon as it ends. Each round ends with
  `debug.FreeOSMemory()`, so the peak of indexing is not kept afterwards. A
  periodic round with nothing to index opens nothing.
* **Memory limit.** When the process runs under a cgroup memory limit (the host
  can set one for plugins), the plugin sets the Go memory limit to 80% of it, so
  the garbage collector works harder near the limit instead of the process being
  killed. An explicit `GOMEMLIMIT` in the environment wins.
* **Indicator.** While a rebuild or an initial index runs, the plugin shows
  "Nginx Log Indexing..." in the processing indicator of NGINX UI and clears it
  when the work is done. The periodic incremental round stays silent.

Set `NGINX_UI_PLUGIN_DEBUG=1` in the plugin environment to write debug lines to
the log.

## Data directory

Everything the plugin writes is below `NGINX_UI_PLUGIN_DATA_DIR`:

| Path | Content |
| --- | --- |
| `index/` | The search index, one folder per log group. |
| `index.db` | The state of each file: position, size, entry count, time range, status. |
| `geolite/` | The IP location database. |
| `http.sock` | The socket the HTTP route of NGINX UI connects to (on Windows a loopback port is used and no file is created). |
| `import/` | Only while an older installation is being taken over. |

The index format is unchanged from the one built into NGINX UI. When the format
of an index from an older version is not readable any more, the plugin discards
that index and builds it again.

## HTTP routes

Relative to `/api/plugins/com.nginxui.log-analytics/http`. Request and response
bodies are the ones the log pages of NGINX UI used, errors keep their numeric
codes.

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/logs/status` | Index state of every log group and the summary |
| POST | `/search`, GET `/entries`, POST `/analytics`, GET `/preflight`, POST `/dashboard` | Search and statistics |
| POST | `/geo/world`, `/geo/regions`, `/geo/points`, `/geo/stats` | Map data |
| POST | `/index/rebuild` | Rebuild one log group (`{"path": "..."}`) or all |
| POST | `/warm` | Open the shards in the background |
| GET | `/geolite/status`, websocket `/geolite/download` | IP location database |
| websocket | `/events` | `nginx_log_index_progress`, `nginx_log_index_complete`, `nginx_log_index_ready`, `processing_status` |

## Takeover from an older installation

NGINX UI writes a handoff before it starts the plugin for the first time on an
installation that used the built-in advanced indexing:

* `import/nginx_log_indices.json`: the records of the old table.
* `import/legacy.json`: `{"index_path": "...", "geolite_path": "..."}`.

The plugin imports the records (keeping their ids, which name the index
folders), moves the old index into `index/` when both are on the same
filesystem and otherwise uses it where it is, copies the location database, and
then deletes `import/`. NGINX UI reads the removal as a finished import, and
until then the handoff can run again after a failure.

## What data leaves the machine

* **To `cloud.nginxui.com`**, only when you download the IP location database:
  a plain request for the database file.
* **Nothing else.** No telemetry, no usage reports. The country database used
  for the world map is embedded in the plugin.

The country database is MaxMind GeoLite2 Country data, used under the
[GeoLite2 terms](https://www.maxmind.com/en/geolite2/eula).

## Packaging

Every platform ships as its own package:

```text
dist/com.nginxui.log-analytics-<version>-linux-amd64.tar.gz
dist/com.nginxui.log-analytics-<version>-linux-amd64.tar.gz.sha256
dist/com.nginxui.log-analytics-<version>-linux-arm64.tar.gz
...
dist/com.nginxui.log-analytics-<version>-windows-arm64.tar.gz
```

Every package holds one binary under `server/dist/`, the web bundle with its
lazily loaded views and static files, the documentation and a `plugin.json`
whose `server.executables` names only that platform, as a per-platform package
must. The committed `plugin.json` keeps all six
platforms; it is what the catalog publishes as the release manifest snapshot.
`go run ./cmd/manifest -platform <goos>-<goarch> -out <file>` writes the
narrowed copy, which is what `build.sh` puts into each archive.

Every package also carries `plugin.sums`, the sha256 of every file but itself
and the signature, and, in a signed package, `plugin.sums.minisig`, the
minisign signature over it. `build.sh` signs only when `MINISIGN_KEY` names a
minisign secret key file:

```bash
MINISIGN_KEY=/path/to/plugin.key ./build.sh
```

Without it the packages are unsigned, and a host installs them only in
developer mode. `MINISIGN_PASSWORD` answers the password prompt without a
terminal.

`build.sh` takes the web bundle from
[plugin-log-analytics-webapp](https://github.com/nginxui/plugin-log-analytics-webapp),
which both log analytics plugins share. `webapp.lock` names the version of its
release, and only changes after a webapp release. The archive is the one
`--webapp` names, the one built in a sibling checkout
(`../plugin-log-analytics-webapp/release`) or the release download, checked
against the `.sha256` file of the release, and the build of this plugin is
unpacked into `webapp/dist`.

## Releasing

Set the version in `cmd/manifest`, regenerate `plugin.json`, move the
`Unreleased` notes in `CHANGELOG.md` under the new version, then push a tag
`v<version>` that matches `plugin.json`. `.github/workflows/release.yml`
takes the webapp release, runs the tests, signs the six packages with the key kept
in the `release` environment and publishes them as a GitHub Release with the
changelog section as its notes.

## Development

```bash
./build.sh --webapp-only                      # the web bundle into webapp/dist
go run ./cmd/manifest                         # regenerate plugin.json
go build ./... && go vet ./...
go test -race -count=1 . ./cmd/... ./internal/...   # long benchmarks skip with -short
./build.sh --host-only                        # build and package the current platform only
./build.sh                                    # cross compile, one package per platform
./build.sh --webapp ARCHIVE                   # package another webapp build
```

The API shape test checks every answer against the types the web pages
declare, read from a checkout of `plugin-log-analytics-webapp` next to this
repository or from `LOG_ANALYTICS_WEBAPP_DIR`.

The plugin depends on the
[plugin-sdk-go](https://github.com/nginxui/plugin-sdk-go) module. To work
against a local checkout of it, create a workspace, which git ignores:

```bash
go work init . ../plugin-sdk-go
go work edit -replace=github.com/nginxui/plugin-sdk-go@v0.1.0=../plugin-sdk-go
```

The state database uses the same GORM dialector as NGINX UI on top of a pure Go
SQLite driver, which is what lets the plugin build without cgo.

## Support

Report problems at <https://github.com/0xJacky/nginx-ui/issues>. Include the
NGINX UI version and the plugin log lines from stderr.

## License

AGPL-3.0. See [LICENSE](LICENSE).

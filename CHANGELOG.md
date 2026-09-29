# Changelog

All notable changes to this plugin are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[semantic versioning](https://semver.org/).

## [Unreleased]

### Added

- First release of the plugin. It takes over what NGINX UI offered as advanced
  log indexing: the search index of the access logs, structured search, the
  traffic dashboard with the visitor map, the log list columns with rebuilding,
  and the IP location database download. The index format is unchanged.
- The plugin takes over an existing installation: the index, its records, the
  settings and the location database that NGINX UI kept before are moved in the
  first time the plugin starts.
- The plugin reads only the nginx log files NGINX UI lists for it, and the
  rotated files next to them.
- Nothing is opened at start. The views ask the plugin to warm up when they
  appear, the plugin opens its index in parallel, and after ten minutes without
  a request it closes the index and drops its caches again. Every indexing round
  hands its memory back to the system, and a memory limit of the process is
  respected by setting the Go memory limit to 80% of it.
- The indexing indicator of NGINX UI shows while a rebuild or an initial index
  runs. The periodic incremental round stays silent.
- Country codes for the world map come from a country database embedded in the
  plugin. The custom location database, the map folder, the indexing interval and
  the number of logs indexed at once are plugin settings.
- The demo mode (`NGINX_UI_DEMO`) makes up provinces and cities for the
  documentation addresses of its synthetic log, as before.

### Fixed

- A full rebuild no longer stops the indexer when it finishes: the restarted
  indexer runs on the context of the services instead of the one of the rebuild.
- Unpacking the IP location database writes to a temporary file first, so an
  indexing round that still uses the previous database keeps working, and it
  never overwrites a custom database.

### Known issues

- Windows: NGINX UI reaches a Windows plugin over a loopback port the plugin has
  to report in the `plugin.initialize` reply (`http_port`). The Go SDK has no way
  to report it yet, so the plugin cannot be used on Windows until it does.

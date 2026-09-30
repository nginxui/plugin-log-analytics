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
- Error logs are indexed beside the access logs and searched in the
  structured view by level, client, request path and text. They have no
  dashboard. Existing indexes of access logs are kept as they are.
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
- The HTTP API is served by the plugin SDK, which listens on the socket in the
  data directory, or on a loopback port it reports to NGINX UI on Windows, so the
  plugin runs on Windows too.
- The IP location database download uses the HTTP proxy configured in NGINX UI.
  NGINX UI passes it to the plugin process through the standard proxy
  environment variables.

### Fixed

- A full rebuild no longer stops the indexer when it finishes: the restarted
  indexer runs on the context of the services instead of the one of the rebuild.
- An unexpected failure is answered with a generic message and its detail goes to
  the plugin log only, instead of returning the raw error text, which could name
  paths on the machine. A request for a log NGINX UI does not list is answered
  with its numbered error.
- Unpacking the IP location database writes to a temporary file first, so an
  indexing round that still uses the previous database keeps working, and it
  never overwrites a custom database.
- Every log line is indexed once, also when the log is rotated. The document id
  follows the content of the file (its first line and the offset of the line),
  not its path, so a file that was renamed, copied by copytruncate or compressed
  is recognized and continues from where the earlier copy ended. Rotated and
  compressed files are read by the incremental round too, a line that is still
  being written waits for its end, and the files of an existing index are read
  once more to replace documents that had the old ids.

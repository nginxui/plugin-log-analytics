#!/usr/bin/env bash
# Build the release artifacts of the log analytics plugin.
#
#   ./build.sh                build and package the current platform
#   ./build.sh --prebuilt DIR package every platform from the executables in DIR
#   ./build.sh --webapp ARCHIVE  package this webapp archive
#   ./build.sh --webapp-only  only take the webapp into webapp/dist
#
# The plugin links SQLite through cgo, as NGINX UI does, so a platform builds
# with a C compiler for it. This script compiles for the current platform
# only; --host-only says the same. The release workflow builds every platform
# with the cross compilers of nginxui/setup-cgo and packages the executables
# with --prebuilt: DIR holds one per platform, named as in the package,
# log-analytics-<goos>-<goarch> with .exe on Windows.
#
# Every platform gets its own package, dist/<id>-<version>-<goos>-<goarch>.tar.gz,
# holding one binary and a plugin.json whose server.executables names only that
# platform, as a per-platform package must. A package with every binary would
# be fifteen times as large and every node would download fourteen binaries it
# never runs. A <archive>.sha256 file sits next to each archive for the catalog.
#
# The browser bundle comes from plugin-log-analytics-webapp, which builds it
# for both log analytics plugins. webapp.lock names the release version. The
# archive is, in this order, the one --webapp names, the one built in a sibling
# checkout (../plugin-log-analytics-webapp/release), or the release download,
# checked against the .sha256 file of the release. The build of this plugin is taken
# into webapp/dist: the packages carry it, including the chunks and the static
# map and country files the bundle loads on demand.
#
# The package layout matches what nginx-ui expects when it installs a plugin:
# plugin.json sits at the root of the archive, next to server/, webapp/ and the
# documentation.
#
# The packages are unsigned. The release workflow signs them with the official
# plugin key through nginxui/plugin-release, which also writes plugin.sums. A
# local build installs on a host in developer mode, or after
# "nginx-ui plugin sign <package> --key <key>".
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIST="${ROOT}/dist"
STAGE="${DIST}/stage"
BIN="${DIST}/bin"

PLUGIN_ID="com.nginxui.log-analytics"
BIN_PREFIX="log-analytics"

usage() {
  echo "usage: $0 [--host-only] [--prebuilt DIR] [--webapp ARCHIVE] [--webapp-only]"
}

PREBUILT=""
WEBAPP_ONLY=0
WEBAPP_ARCHIVE="${WEBAPP_ARCHIVE:-}"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --host-only) ;;
    --prebuilt)
      if [[ $# -lt 2 ]]; then
        usage >&2
        exit 2
      fi
      PREBUILT="$(cd "$2" && pwd)"
      shift
      ;;
    --webapp-only) WEBAPP_ONLY=1 ;;
    --webapp)
      if [[ $# -lt 2 ]]; then
        usage >&2
        exit 2
      fi
      WEBAPP_ARCHIVE="$2"
      shift
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      exit 2
      ;;
  esac
  shift
done

cd "${ROOT}"

# The webapp of this plugin, taken from the release archive into webapp/dist.
WEBAPP_VERSION="$(sed -n 's/^version=//p' webapp.lock)"
WEBAPP_NAME="plugin-log-analytics-webapp-${WEBAPP_VERSION}.tar.gz"
if [[ -z "${WEBAPP_ARCHIVE}" && -f "../plugin-log-analytics-webapp/release/${WEBAPP_NAME}" ]]; then
  WEBAPP_ARCHIVE="../plugin-log-analytics-webapp/release/${WEBAPP_NAME}"
fi
if [[ -z "${WEBAPP_ARCHIVE}" ]]; then
  # Kept apart from the packages, dist/*.tar.gz is what a release publishes.
  # A later run reuses the download while it matches the checksum kept with it.
  WEBAPP_ARCHIVE="${DIST}/cache/${WEBAPP_NAME}"
  if [[ ! -f "${WEBAPP_ARCHIVE}.sha256" || ! -f "${WEBAPP_ARCHIVE}" ]] ||
    [[ "$(shasum -a 256 "${WEBAPP_ARCHIVE}" | cut -d' ' -f1)" != "$(cat "${WEBAPP_ARCHIVE}.sha256")" ]]; then
    url="https://github.com/nginxui/plugin-log-analytics-webapp/releases/download/v${WEBAPP_VERSION}/${WEBAPP_NAME}"
    mkdir -p "${DIST}/cache"
    rm -f "${WEBAPP_ARCHIVE}.sha256"
    # Retries ride out a passing server error of the download.
    curl -fsSL --retry 5 --retry-delay 3 -o "${WEBAPP_ARCHIVE}" "${url}"
    # The release publishes the checksum next to the archive
    expected="$(curl -fsSL --retry 5 --retry-delay 3 "${url}.sha256" | cut -d' ' -f1)"
    actual="$(shasum -a 256 "${WEBAPP_ARCHIVE}" | cut -d' ' -f1)"
    if [[ -z "${expected}" || "${actual}" != "${expected}" ]]; then
      echo "the webapp archive does not match the checksum of its release: ${actual}" >&2
      exit 1
    fi
    printf '%s' "${expected}" >"${WEBAPP_ARCHIVE}.sha256"
  fi
fi
if [[ ! -f "${WEBAPP_ARCHIVE}" ]]; then
  echo "no webapp archive at ${WEBAPP_ARCHIVE}" >&2
  exit 1
fi
echo "webapp: ${WEBAPP_ARCHIVE}"
rm -rf webapp/dist "${DIST}/webapp"
mkdir -p webapp "${DIST}/webapp"
tar -xzf "${WEBAPP_ARCHIVE}" -C "${DIST}/webapp" "${PLUGIN_ID}"
mv "${DIST}/webapp/${PLUGIN_ID}" webapp/dist
rm -rf "${DIST}/webapp"

if [[ ! -f plugin.json ]]; then
  echo "plugin.json is missing" >&2
  exit 1
fi

# The packages carry the browser bundle, so it has to exist.
WEBAPP_FILES=(
  main.js
  style.css
  icon.svg
  chunks/search.js
  chunks/dashboard.js
)
for file in "${WEBAPP_FILES[@]}"; do
  if [[ ! -f "webapp/dist/${file}" ]]; then
    echo "webapp/dist/${file} is missing from the webapp archive" >&2
    exit 1
  fi
done
if [[ "${WEBAPP_ONLY}" == 1 ]]; then
  exit 0
fi

# The version is read from the manifest so it has one source.
VERSION="$(sed -n 's/^  "version": "\(.*\)",$/\1/p' plugin.json | head -n 1)"
if [[ -z "${VERSION}" ]]; then
  echo "could not read the version from plugin.json" >&2
  exit 1
fi

# The platforms Nginx UI is released for. The host names a platform by GOOS
# and GOARCH only, so one linux-arm package serves ARMv5 to ARMv7: it is built
# for ARMv5, which the later ones run.
PLATFORMS=(
  "linux/amd64"
  "linux/arm64"
  "linux/386"
  "linux/arm"
  "linux/riscv64"
  "linux/loong64"
  "linux/mips"
  "linux/mipsle"
  "linux/mips64"
  "linux/mips64le"
  "darwin/amd64"
  "darwin/arm64"
  "windows/amd64"
  "windows/arm64"
  "windows/386"
)

if [[ -z "${PREBUILT}" ]]; then
  PLATFORMS=("$(go env GOOS)/$(go env GOARCH)")
fi

# Keep macOS tar from adding AppleDouble "._*" entries for extended attributes.
export COPYFILE_DISABLE=1

# sha256_hex prints the lowercase hex sha256 of one file. The file is read from
# stdin so its name cannot change the output.
sha256_hex() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum <"$1" | cut -d ' ' -f 1
  else
    shasum -a 256 <"$1" | cut -d ' ' -f 1
  fi
}

# sha256_line prints "<digest>  <file name>", the format sha256sum -c reads.
sha256_line() {
  local file="$1"
  printf '%s  %s\n' "$(sha256_hex "${file}")" "$(basename "${file}")"
}

# binary_name is the packaged file name of one platform's executable.
binary_name() {
  local goos="$1" goarch="$2"
  local name="${BIN_PREFIX}-${goos}-${goarch}"
  if [[ "${goos}" == "windows" ]]; then
    name="${name}.exe"
  fi
  echo "${name}"
}

# stage_common copies what every package ships besides the binaries.
stage_common() {
  local dir="$1"
  # The manifest fragment only feeds the tests and is not part of a package.
  mkdir -p "${dir}/webapp/dist"
  cp -R "${ROOT}/webapp/dist/." "${dir}/webapp/dist/"
  rm -f "${dir}/webapp/dist/manifest.webapp.json"
  for doc in README.md LICENSE; do
    if [[ -f "${ROOT}/${doc}" ]]; then
      cp "${ROOT}/${doc}" "${dir}/${doc}"
    fi
  done
}

# package_dir writes one archive with plugin.json as its first entry. The top level entries are listed explicitly
# so the archive has no "./" root entry.
package_dir() {
  local dir="$1" archive="$2"
  local entries=(plugin.json)
  for entry in README.md LICENSE server webapp; do
    if [[ -e "${dir}/${entry}" ]]; then
      entries+=("${entry}")
    fi
  done
  rm -f "${archive}" "${archive}.sha256"
  tar -czf "${archive}" -C "${dir}" "${entries[@]}"
  sha256_line "${archive}" >"${archive}.sha256"
  OUTPUTS+=("${archive}" "${archive}.sha256")
}

rm -rf "${STAGE}" "${BIN}" "${DIST}/pkg"
rm -f "${DIST}/${PLUGIN_ID}-${VERSION}"*.tar.gz "${DIST}/${PLUGIN_ID}-${VERSION}"*.tar.gz.sha256
mkdir -p "${STAGE}" "${BIN}"

# The manifest tool narrows plugin.json to one platform per package.
MANIFEST_TOOL="${DIST}/.manifest-tool"
go build -o "${MANIFEST_TOOL}" ./cmd/manifest

echo "building ${PLUGIN_ID} ${VERSION}"

OUTPUTS=()
for platform in "${PLATFORMS[@]}"; do
  goos="${platform%%/*}"
  goarch="${platform##*/}"
  key="${goos}-${goarch}"
  name="$(binary_name "${goos}" "${goarch}")"

  echo "  ${goos}/${goarch}"
  if [[ -n "${PREBUILT}" ]]; then
    if [[ ! -f "${PREBUILT}/${name}" ]]; then
      echo "${PREBUILT}/${name} is missing" >&2
      exit 1
    fi
    cp "${PREBUILT}/${name}" "${BIN}/${name}"
    chmod +x "${BIN}/${name}"
  else
    CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o "${BIN}/${name}" .
  fi
  echo "    ${name} ($(du -h "${BIN}/${name}" | cut -f1 | tr -d '[:space:]'))"

  dir="${STAGE}/${key}"
  mkdir -p "${dir}/server/dist"
  cp "${BIN}/${name}" "${dir}/server/dist/${name}"
  stage_common "${dir}"
  "${MANIFEST_TOOL}" -in "${ROOT}/plugin.json" -platform "${key}" -out "${dir}/plugin.json" >/dev/null

  package_dir "${dir}" "${DIST}/${PLUGIN_ID}-${VERSION}-${key}.tar.gz"
done

# Every binary also sits in its stage directory, which stays for inspection.
rm -rf "${MANIFEST_TOOL}" "${BIN}"

echo "packages:"
for file in "${OUTPUTS[@]}"; do
  printf "  %-6s %s\n" "$(du -h "${file}" | cut -f1 | tr -d '[:space:]')" "${file#"${ROOT}/"}"
done

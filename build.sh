#!/usr/bin/env bash
# Build the release artifacts of the log analytics plugin.
#
#   ./build.sh              build every supported platform, one package each
#   ./build.sh --host-only  build and package the current platform only
#
# Every platform gets its own package, dist/<id>-<version>-<goos>-<goarch>.tar.gz,
# holding one binary and a plugin.json whose server.executables names only that
# platform (plugin spec PKG-12). A package with all six binaries would be six
# times as large and every node would download five binaries it never runs. A
# <archive>.sha256 file sits next to each archive for the catalog.
#
# The browser bundle must be built first (cd webapp && bun install && bun run
# build): the packages carry webapp/dist, including the chunks and the static
# map and country files the bundle loads on demand.
#
# The package layout matches what nginx-ui expects when it installs a plugin:
# plugin.json sits at the root of the archive, next to server/, webapp/ and the
# documentation.
#
# Every package also carries plugin.sums at its root: the sha256 of each file
# of the package in sha256sum format, sorted by path. When MINISIGN_KEY names a
# minisign secret key file, plugin.sums is signed into plugin.sums.minisig and
# nginx-ui derives the trust level from the signing key. Without MINISIGN_KEY
# the packages are unsigned, and a host installs them only in developer mode.
#
#   MINISIGN_KEY=/path/to/plugin.key ./build.sh
#
# minisign asks for the key password once per package. MINISIGN_PASSWORD
# answers the prompt without a terminal, which is how the release workflow
# signs; a key created without a password (minisign -G -W) never asks.
#
#   MINISIGN_KEY=/path/to/plugin.key MINISIGN_PASSWORD=... ./build.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIST="${ROOT}/dist"
STAGE="${DIST}/stage"
BIN="${DIST}/bin"

PLUGIN_ID="com.nginxui.log-analytics"
BIN_PREFIX="log-analytics"

usage() {
  echo "usage: $0 [--host-only]"
}

HOST_ONLY=0
for arg in "$@"; do
  case "${arg}" in
    --host-only) HOST_ONLY=1 ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      exit 2
      ;;
  esac
done

# The key path is resolved before the cd below, so a relative path works.
MINISIGN_KEY="${MINISIGN_KEY:-}"
MINISIGN_PASSWORD="${MINISIGN_PASSWORD:-}"
if [[ -n "${MINISIGN_KEY}" ]]; then
  if [[ ! -f "${MINISIGN_KEY}" ]]; then
    echo "MINISIGN_KEY does not name a file: ${MINISIGN_KEY}" >&2
    exit 1
  fi
  if ! command -v minisign >/dev/null 2>&1; then
    echo "MINISIGN_KEY is set but minisign is not installed" >&2
    exit 1
  fi
  MINISIGN_KEY="$(cd "$(dirname "${MINISIGN_KEY}")" && pwd)/$(basename "${MINISIGN_KEY}")"
fi

cd "${ROOT}"

if [[ ! -f plugin.json ]]; then
  echo "plugin.json is missing, run: go run ./cmd/manifest" >&2
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
    echo "webapp/dist/${file} is missing, build the webapp first: cd webapp && bun install && bun run build" >&2
    exit 1
  fi
done

# The version is read back from the generated manifest so it has one source.
VERSION="$(sed -n 's/^  "version": "\(.*\)",$/\1/p' plugin.json | head -n 1)"
if [[ -z "${VERSION}" ]]; then
  echo "could not read the version from plugin.json" >&2
  exit 1
fi

PLATFORMS=(
  "linux/amd64"
  "linux/arm64"
  "darwin/amd64"
  "darwin/arm64"
  "windows/amd64"
  "windows/arm64"
)

if [[ "${HOST_ONLY}" -eq 1 ]]; then
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
  # The manifest fragment only feeds cmd/manifest and is not part of a package.
  mkdir -p "${dir}/webapp/dist"
  cp -R "${ROOT}/webapp/dist/." "${dir}/webapp/dist/"
  rm -f "${dir}/webapp/dist/manifest.webapp.json"
  for doc in README.md LICENSE CHANGELOG.md; do
    if [[ -f "${ROOT}/${doc}" ]]; then
      cp "${ROOT}/${doc}" "${dir}/${doc}"
    fi
  done
}

# write_sums writes plugin.sums at the root of a staged package: one
# "<sha256>  <path>" line per regular file, with the path relative to the root
# and the lines sorted bytewise by path. plugin.sums and plugin.sums.minisig
# are not listed. The list is written next to the directory first so find
# never sees it.
write_sums() {
  local dir="$1" file
  rm -f "${dir}/plugin.sums" "${dir}/plugin.sums.minisig"
  (
    cd "${dir}"
    find . -type f | sed 's|^\./||' | LC_ALL=C sort | while IFS= read -r file; do
      printf '%s  %s\n' "$(sha256_hex "${file}")" "${file}"
    done
  ) >"${dir}.sums"
  mv "${dir}.sums" "${dir}/plugin.sums"
}

# sign_sums signs plugin.sums into plugin.sums.minisig when MINISIGN_KEY is set.
sign_sums() {
  local dir="$1"
  if [[ -z "${MINISIGN_KEY}" ]]; then
    return 0
  fi
  (
    cd "${dir}"
    if [[ -n "${MINISIGN_PASSWORD}" ]]; then
      printf '%s\n' "${MINISIGN_PASSWORD}" \
        | minisign -S -m plugin.sums -x plugin.sums.minisig -s "${MINISIGN_KEY}" -t "${PLUGIN_ID} ${VERSION}"
    else
      minisign -S -m plugin.sums -x plugin.sums.minisig -s "${MINISIGN_KEY}" -t "${PLUGIN_ID} ${VERSION}"
    fi
  )
}

# package_dir writes one archive with plugin.json as its first entry and the
# signature files right after it. The top level entries are listed explicitly
# so the archive has no "./" root entry.
package_dir() {
  local dir="$1" archive="$2"
  local entries=(plugin.json plugin.sums)
  for entry in plugin.sums.minisig README.md LICENSE CHANGELOG.md server webapp; do
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
GOWORK=off go build -o "${MANIFEST_TOOL}" ./cmd/manifest

echo "building ${PLUGIN_ID} ${VERSION}"
if [[ -z "${MINISIGN_KEY}" ]]; then
  echo "MINISIGN_KEY is not set, the packages are unsigned"
fi

OUTPUTS=()
for platform in "${PLATFORMS[@]}"; do
  goos="${platform%%/*}"
  goarch="${platform##*/}"
  key="${goos}-${goarch}"
  name="$(binary_name "${goos}" "${goarch}")"

  echo "  ${goos}/${goarch}"
  GOWORK=off CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" \
    go build -trimpath -ldflags "-s -w" -o "${BIN}/${name}" .
  echo "    ${name} ($(du -h "${BIN}/${name}" | cut -f1 | tr -d '[:space:]'))"

  dir="${STAGE}/${key}"
  mkdir -p "${dir}/server/dist"
  cp "${BIN}/${name}" "${dir}/server/dist/${name}"
  stage_common "${dir}"
  "${MANIFEST_TOOL}" -in "${ROOT}/plugin.json" -platform "${key}" -out "${dir}/plugin.json" >/dev/null

  write_sums "${dir}"
  sign_sums "${dir}"
  package_dir "${dir}" "${DIST}/${PLUGIN_ID}-${VERSION}-${key}.tar.gz"
done

# Every binary also sits in its stage directory, which stays for inspection.
rm -rf "${MANIFEST_TOOL}" "${BIN}"

echo "packages:"
for file in "${OUTPUTS[@]}"; do
  printf "  %-6s %s\n" "$(du -h "${file}" | cut -f1 | tr -d '[:space:]')" "${file#"${ROOT}/"}"
done

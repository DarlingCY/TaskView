#!/usr/bin/env bash
#
# Builds the multi-platform TaskView CLI archives and records their checksums.
#
# The script produces exactly the same artifacts as scripts/build-cli.ps1:
#
#   dist/taskview-cli-<version>-<os>-<arch>.zip       (windows)
#   dist/taskview-cli-<version>-<os>-<arch>.tar.gz    (linux, darwin)
#   dist/taskview-cli.exe                             (windows/amd64)
#   dist/taskview-skill-<version>.zip                  (self-contained skill)
#   dist/SHA256SUMS                                   (aggregated checksums)
#   skills/task-view/scripts/<os>-<arch>/taskview-cli[.exe]
#
# The checksum list keeps the entries of other files (the Windows installer
# for example), replaces the entries of the files it wrote and is updated
# atomically, so a failed build never leaves a half written list behind.
#
# Missing tools are reported, never installed.

set -euo pipefail

VERSION="${1:-2.1.0}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="$REPO_ROOT/dist"
CHECKSUM_FILE="$DIST_DIR/SHA256SUMS"
SKILL_DIR="$REPO_ROOT/skills/task-view"

fail() {
  echo "error: $*" >&2
  exit 1
}

command -v go >/dev/null 2>&1 || fail "the Go toolchain is required but was not found in PATH. Install Go and try again."

# The checksum tool has to exist on this host: sha256sum on Linux, shasum on
# macOS. Nothing is installed here.
SHA256SUM=""
SHASUM=""
if command -v sha256sum >/dev/null 2>&1; then
  SHA256SUM="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  SHASUM="shasum"
else
  fail "neither sha256sum nor shasum was found in PATH. One of them is required to write $CHECKSUM_FILE."
fi

command -v tar >/dev/null 2>&1 || fail "tar is required to build the Linux and macOS archives but was not found in PATH."
command -v zip >/dev/null 2>&1 || fail "zip is required to build the Windows archives but was not found in PATH."

# sha256_of prints the checksum of one file.
sha256_of() {
  local file="$1"
  local sum
  if [ -n "$SHA256SUM" ]; then
    sum="$("$SHA256SUM" "$file")"
  else
    sum="$("$SHASUM" -a 256 "$file")"
  fi
  sum="${sum%% *}"
  [ -n "$sum" ] || fail "could not calculate the checksum of $file"
  printf '%s' "$sum"
}

mkdir -p "$DIST_DIR"

TARGETS=(
  "windows amd64 .exe"
  "windows arm64 .exe"
  "linux amd64 "
  "linux arm64 "
  "darwin amd64 "
  "darwin arm64 "
)

export CGO_ENABLED=0

ENTRIES=()

for target in "${TARGETS[@]}"; do
  read -r GOOS GOARCH EXT <<< "$target"
  BIN_NAME="taskview-cli${EXT}"
  STAGE_DIR="$DIST_DIR/stage_${GOOS}_${GOARCH}"
  mkdir -p "$STAGE_DIR"
  OUT_BIN="$STAGE_DIR/$BIN_NAME"

  echo "==> Building taskview-cli for $GOOS/$GOARCH"
  if ! GOOS="$GOOS" GOARCH="$GOARCH" go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$OUT_BIN" ./cmd/taskview-cli; then
    fail "go build failed for $GOOS/$GOARCH"
  fi

  if [ "$GOOS" = "windows" ] && [ "$GOARCH" = "amd64" ]; then
    cp "$OUT_BIN" "$DIST_DIR/taskview-cli.exe"
  fi

  if [ "$GOOS" = "windows" ]; then
    ARCHIVE_NAME="taskview-cli-$VERSION-$GOOS-$GOARCH.zip"
    ARCHIVE_PATH="$DIST_DIR/$ARCHIVE_NAME"
    rm -f "$ARCHIVE_PATH"
    if ! (cd "$STAGE_DIR" && zip -q -9 "$ARCHIVE_PATH" "$BIN_NAME"); then
      fail "zip failed for $GOOS/$GOARCH"
    fi
  else
    ARCHIVE_NAME="taskview-cli-$VERSION-$GOOS-$GOARCH.tar.gz"
    ARCHIVE_PATH="$DIST_DIR/$ARCHIVE_NAME"
    rm -f "$ARCHIVE_PATH"
    if ! tar -czf "$ARCHIVE_PATH" -C "$STAGE_DIR" "$BIN_NAME"; then
      fail "tar failed for $GOOS/$GOARCH"
    fi
  fi

  SKILL_BIN_DIR="$SKILL_DIR/scripts/$GOOS-$GOARCH"
  mkdir -p "$SKILL_BIN_DIR"
  cp "$OUT_BIN" "$SKILL_BIN_DIR/$BIN_NAME"

  rm -rf "$STAGE_DIR"
  ENTRIES+=("$(sha256_of "$ARCHIVE_PATH")  $ARCHIVE_NAME")
  echo "Created $ARCHIVE_NAME"
done

SKILL_ARCHIVE="taskview-skill-$VERSION.zip"
rm -f "$DIST_DIR/$SKILL_ARCHIVE"
if ! (cd "$(dirname "$SKILL_DIR")" && zip -q -9 -r "$DIST_DIR/$SKILL_ARCHIVE" task-view); then
  fail "zip failed for the skill bundle"
fi
ENTRIES+=("$(sha256_of "$DIST_DIR/$SKILL_ARCHIVE")  $SKILL_ARCHIVE")

# The checksum list is aggregated: the entries of the files that were not
# written by this build are kept, the entries of the archives above are
# replaced. The new list is written to a temporary file and moved into place,
# so the list is never half written.
TMP_CHECKSUM="$CHECKSUM_FILE.tmp.$$"
trap 'rm -f "$TMP_CHECKSUM"' EXIT
: > "$TMP_CHECKSUM"

if [ -f "$CHECKSUM_FILE" ]; then
  while IFS= read -r line || [ -n "$line" ]; do
    # A list written on Windows may use CRLF. Drop the carriage return so
    # files are compared by name, not by name plus "\r".
    line="${line%$'\r'}"
    case "$line" in
      ''|'#'*) continue ;;
    esac
    read -r _checksum name <<< "$line"
    name="${name#\*}"
    replaced=0
    for entry in "${ENTRIES[@]}"; do
      read -r _entry_checksum entry_name <<< "$entry"
      entry_name="${entry_name#\*}"
      if [ "$name" = "$entry_name" ]; then
        replaced=1
        break
      fi
    done
    if [ "$replaced" -eq 0 ]; then
      printf '%s\n' "$line" >> "$TMP_CHECKSUM"
    fi
  done < "$CHECKSUM_FILE"
fi

for entry in "${ENTRIES[@]}"; do
  printf '%s\n' "$entry" >> "$TMP_CHECKSUM"
done

mv -f "$TMP_CHECKSUM" "$CHECKSUM_FILE"

echo "Wrote $CHECKSUM_FILE"
echo "Done building CLI."

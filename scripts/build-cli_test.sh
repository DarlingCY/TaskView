#!/usr/bin/env bash
#
# Regression checks for scripts/build-cli.sh.
#
# The checks use stand-in "go" and "zip" commands and write fake archives, so
# they run on a host without a Go toolchain or zip and they do not claim to
# have produced real cross-platform binaries. Real tar and sha256sum are used
# so the checksum aggregation is exercised for real.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILD_SCRIPT="$REPO_ROOT/scripts/build-cli.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

make_fake_repo() {
  local repo="$1"
  mkdir -p "$repo/scripts" "$repo/cmd/taskview-cli" "$repo/dist" "$repo/mockbin" "$repo/skills/task-view"
  cp "$BUILD_SCRIPT" "$repo/scripts/build-cli.sh"
  printf 'skill fixture\n' > "$repo/skills/task-view/SKILL.md"
  printf 'skill readme\n' > "$repo/skills/task-view/README.md"
  printf 'module example.test/taskview\n\ngo 1.26\n' > "$repo/go.mod"
  printf 'package main\n\nfunc main() {}\n' > "$repo/cmd/taskview-cli/main.go"

  cat > "$repo/mockbin/go" <<'MOCK_GO'
#!/usr/bin/env bash
set -euo pipefail
out=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    *) shift ;;
  esac
done
[ -n "$out" ] || { echo "mock go: missing -o" >&2; exit 1; }
printf 'mock binary for %s/%s\n' "${GOOS:-}" "${GOARCH:-}" > "$out"
MOCK_GO

  cat > "$repo/mockbin/zip" <<'MOCK_ZIP'
#!/usr/bin/env bash
set -euo pipefail
archive=""
member=""
for arg in "$@"; do
  case "$arg" in
    -*) ;;
    *)
      if [ -z "$archive" ]; then
        archive="$arg"
      elif [ -z "$member" ]; then
        member="$arg"
      fi
      ;;
  esac
done
[ -n "$archive" ] && [ -n "$member" ] || { echo "mock zip: bad arguments" >&2; exit 1; }
printf 'mock zip member %s\n' "$member" > "$archive"
MOCK_ZIP

  chmod +x "$repo/mockbin/go" "$repo/mockbin/zip"

  cat > "$repo/dist/SHA256SUMS" <<'SUMS'
1111111111111111111111111111111111111111111111111111111111111111  TaskView-Setup-9.9.9-windows-amd64.exe
3333333333333333333333333333333333333333333333333333333333333333  keep-me.txt
SUMS
}

make_minimal_bin() {
  local bin="$1"
  mkdir -p "$bin"
  local tool
  for tool in bash dirname mkdir cp rm mv tar gzip; do
    ln -s "/usr/bin/$tool" "$bin/$tool"
  done
}

run_build() {
  local repo="$1"
  local path_prefix="$2"
  PATH="$path_prefix:/usr/bin:/bin" bash "$repo/scripts/build-cli.sh" 9.9.9
}

check_cli_entries() {
  local dist="$1"
  local skill_dir="$(dirname "$dist")/skills/task-view"
  local target bin_name
  for target in windows-amd64 windows-arm64 linux-amd64 linux-arm64 darwin-amd64 darwin-arm64; do
    bin_name="taskview-cli"
    case "$target" in windows-*) bin_name="taskview-cli.exe" ;; esac
    [ -f "$skill_dir/scripts/$target/$bin_name" ] || fail "missing bundled CLI for $target"
    grep -q "mock binary for ${target/-//}" "$skill_dir/scripts/$target/$bin_name" ||
      fail "bundled CLI has the wrong target for $target"
  done
  [ -f "$dist/taskview-skill-9.9.9.zip" ] || fail "missing skill archive"
  grep -q 'task-view' "$dist/taskview-skill-9.9.9.zip" || fail "skill archive has the wrong root"
  [ "$(grep -c 'taskview-skill-9.9.9.zip' "$dist/SHA256SUMS")" -eq 1 ] ||
    fail "expected one skill checksum entry"
  (cd "$dist" && grep 'taskview-skill-9.9.9.zip' SHA256SUMS | sha256sum -c - >/dev/null) ||
    fail "the skill checksum does not match the archive"
  [ "$(grep -c 'taskview-cli-9.9.9-' "$dist/SHA256SUMS")" -eq 6 ] ||
    fail "expected six CLI checksum entries"
  grep -q 'TaskView-Setup-9.9.9-windows-amd64.exe' "$dist/SHA256SUMS" ||
    fail "the installer entry was not preserved"
  grep -q 'keep-me.txt' "$dist/SHA256SUMS" ||
    fail "an unrelated checksum entry was not preserved"
  grep -q 'taskview-cli-9.9.9-windows-amd64.zip' "$dist/SHA256SUMS" ||
    fail "the windows/amd64 archive entry is missing"
  if grep -q '^2222222222222222222222222222222222222222222222222222222222222222' "$dist/SHA256SUMS"; then
    fail "the stale CLI checksum entry was kept"
  fi
  (cd "$dist" && grep 'taskview-cli-9.9.9-' SHA256SUMS | sha256sum -c - >/dev/null) ||
    fail "the CLI checksums do not match the archives"
  [ -f "$dist/taskview-cli.exe" ] || fail "dist/taskview-cli.exe was not produced"
  if find "$dist" -maxdepth 1 -type d -name 'stage_*' | grep -q .; then
    fail "a staging directory was left behind"
  fi
}

# 1. Aggregation: keep unrelated entries, replace stale same-name CLI entries
#    and write checksums that match the produced archives.
repo="$WORK/aggregate"
make_fake_repo "$repo"
cat > "$repo/dist/SHA256SUMS" <<'SUMS'
1111111111111111111111111111111111111111111111111111111111111111  TaskView-Setup-9.9.9-windows-amd64.exe
2222222222222222222222222222222222222222222222222222222222222222  taskview-cli-9.9.9-linux-amd64.tar.gz
3333333333333333333333333333333333333333333333333333333333333333  keep-me.txt
SUMS
run_build "$repo" "$repo/mockbin" >/dev/null
check_cli_entries "$repo/dist"

# Running the build again must not duplicate the entries.
run_build "$repo" "$repo/mockbin" >/dev/null
check_cli_entries "$repo/dist"
echo "ok: checksums are idempotent and skill bundles contain all six CLI targets"

# 2. The shasum fallback is used when sha256sum is unavailable.
repo="$WORK/shasum"
make_fake_repo "$repo"
make_minimal_bin "$repo/minbin"
cp "$repo/mockbin/go" "$repo/minbin/go"
cp "$repo/mockbin/zip" "$repo/minbin/zip"
cat > "$repo/minbin/shasum" <<'MOCK_SHASUM'
#!/bin/bash
set -euo pipefail
[ "$1" = "-a" ] && [ "$2" = "256" ] || { echo "mock shasum: expected -a 256" >&2; exit 1; }
shift 2
exec /usr/bin/sha256sum "$@"
MOCK_SHASUM
chmod +x "$repo/minbin/go" "$repo/minbin/zip" "$repo/minbin/shasum"
PATH="$repo/minbin" bash "$repo/scripts/build-cli.sh" 9.9.9 >/dev/null
check_cli_entries "$repo/dist"
echo "ok: shasum -a 256 fallback works"

# 3. Missing checksum tool: a clear error, not a silently missing file.
repo="$WORK/no-checksum"
make_fake_repo "$repo"
make_minimal_bin "$repo/minbin"
cp "$repo/mockbin/go" "$repo/minbin/go"
cp "$repo/mockbin/zip" "$repo/minbin/zip"
chmod +x "$repo/minbin/go" "$repo/minbin/zip"
set +e
PATH="$repo/minbin" bash "$repo/scripts/build-cli.sh" 9.9.9 >"$repo/out" 2>"$repo/err"
status=$?
set -e
[ "$status" -ne 0 ] || fail "the build succeeded without a checksum tool"
grep -q 'neither sha256sum nor shasum' "$repo/err" ||
  fail "the missing checksum tool error was not reported"
echo "ok: a missing checksum tool is reported"

# 4. Missing archive tool: a clear error before any archive is written.
repo="$WORK/no-zip"
make_fake_repo "$repo"
make_minimal_bin "$repo/minbin"
cp "$repo/mockbin/go" "$repo/minbin/go"
ln -s /usr/bin/sha256sum "$repo/minbin/sha256sum"
set +e
PATH="$repo/minbin" bash "$repo/scripts/build-cli.sh" 9.9.9 >"$repo/out" 2>"$repo/err"
status=$?
set -e
[ "$status" -ne 0 ] || fail "the build succeeded without zip"
grep -q 'zip is required' "$repo/err" ||
  fail "the missing zip error was not reported"
echo "ok: a missing archive tool is reported"

# 4b. Missing Go toolchain: a clear error, not a half-built dist.
repo="$WORK/no-go"
make_fake_repo "$repo"
make_minimal_bin "$repo/minbin"
cp "$repo/mockbin/zip" "$repo/minbin/zip"
ln -s /usr/bin/sha256sum "$repo/minbin/sha256sum"
set +e
PATH="$repo/minbin" bash "$repo/scripts/build-cli.sh" 9.9.9 >"$repo/out" 2>"$repo/err"
status=$?
set -e
[ "$status" -ne 0 ] || fail "the build succeeded without go"
grep -q 'Go toolchain is required' "$repo/err" ||
  fail "the missing go error was not reported"
[ -f "$repo/dist/SHA256SUMS" ] || fail "the previous checksum list disappeared"
grep -q 'TaskView-Setup-9.9.9-windows-amd64.exe' "$repo/dist/SHA256SUMS" ||
  fail "the installer entry was dropped by a failed build"
echo "ok: a missing Go toolchain is reported and the checksum list is untouched"

# 4c. Missing tar: the Linux/macOS archives cannot be built, so nothing is
# committed to the checksum list.
repo="$WORK/no-tar"
make_fake_repo "$repo"
mkdir -p "$repo/minbin"
for tool in bash dirname mkdir cp rm mv gzip sha256sum; do
  ln -s "/usr/bin/$tool" "$repo/minbin/$tool"
done
for tool in go zip; do cp "$repo/mockbin/$tool" "$repo/minbin/$tool"; done
chmod +x "$repo/minbin/go" "$repo/minbin/zip"
set +e
PATH="$repo/minbin" bash "$repo/scripts/build-cli.sh" 9.9.9 >"$repo/out" 2>"$repo/err"
status=$?
set -e
[ "$status" -ne 0 ] || fail "the build succeeded without tar"
grep -q 'tar is required' "$repo/err" ||
  fail "the missing tar error was not reported"
echo "ok: a missing tar is reported"

# 5. A failing archive step keeps the previous checksum list intact: the list
# is written to a temporary file and moved into place only after every
# archive exists.
repo="$WORK/atomic"
make_fake_repo "$repo"
cp "$repo/dist/SHA256SUMS" "$repo/expected"
cat > "$repo/mockbin/zip" <<'MOCK_ZIP_FAIL'
#!/usr/bin/env bash
exit 7
MOCK_ZIP_FAIL
chmod +x "$repo/mockbin/zip"
set +e
run_build "$repo" "$repo/mockbin" >"$repo/out" 2>"$repo/err"
status=$?
set -e
[ "$status" -ne 0 ] || fail "the build succeeded although zip failed"
cmp -s "$repo/expected" "$repo/dist/SHA256SUMS" ||
  fail "a failed build changed the checksum list"
if find "$repo/dist" -maxdepth 1 -name 'SHA256SUMS.tmp.*' | grep -q .; then
  fail "a temporary checksum file was left behind"
fi
echo "ok: a failed build leaves the checksum list unchanged"

echo "build-cli.sh regression checks passed"

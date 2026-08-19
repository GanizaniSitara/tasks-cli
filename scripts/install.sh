#!/usr/bin/env bash
# install.sh -- build tasks-cli and install it to the one place macOS looks.
#
# The README used to say `go build -o tasks-cli ./cmd/tasks-cli`, which leaves a
# second binary beside the source. That copy is on nobody's PATH but is easy to
# run by accident, and it ages silently while the installed one moves on. Build
# through this script instead: there is exactly one installed binary per host,
# and `tasks-cli version` reports the commit it came from.
set -euo pipefail

DESTINATION="${1:-$HOME/.local/bin/tasks-cli}"
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"

if [ "${SKIP_TESTS:-}" != "1" ]; then
  echo "running tests..."
  go test ./...
fi

commit="$(git rev-parse --short HEAD)"
if [ -n "$(git status --porcelain)" ]; then
  commit="${commit}-dirty"
fi
built="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# Build to a temp file first so a failed build never leaves a half-written
# binary on PATH, then move it into place atomically.
staging="$(mktemp -t tasks-cli.build)"
trap 'rm -f "$staging"' EXIT
go build -ldflags "-X main.buildCommit=${commit} -X main.buildTime=${built}" -o "$staging" ./cmd/tasks-cli

mkdir -p "$(dirname "$DESTINATION")"
mv -f "$staging" "$DESTINATION"
chmod +x "$DESTINATION"
trap - EXIT

echo "installed ${commit} -> ${DESTINATION}"
"$DESTINATION" version

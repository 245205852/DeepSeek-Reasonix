#!/usr/bin/env bash
# The Wails CLI and the Wails library must be the same version.
#
# They drift silently in both directions. Dependabot bumps the library in
# desktop/go.mod but cannot see the CLI, whose version is a string inside three
# workflow files; and `wails dev` rewrites desktop/go.mod down to whatever CLI
# the developer has installed, so a local run can quietly revert a dependency
# upgrade. One pin, read by everyone, and this check to prove it held.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
pin="$(tr -d '[:space:]' < "$root/.wails-version")"
mod="$(awk '/github.com\/wailsapp\/wails\/v2 v/ {print $2; exit}' "$root/desktop/go.mod")"

if [ -z "$pin" ]; then
  echo "check-wails-pin: .wails-version is empty" >&2
  exit 1
fi
if [ "$pin" != "$mod" ]; then
  cat >&2 <<MSG
check-wails-pin: version drift
  .wails-version   $pin
  desktop/go.mod   $mod

Running \`wails dev\` with an older CLI rewrites desktop/go.mod to match it. If the
library upgrade is intended, bump .wails-version too so CI installs the same CLI;
otherwise restore go.mod (git checkout -- desktop/go.mod desktop/go.sum).
MSG
  exit 1
fi

for workflow in "$root"/.github/workflows/ci.yml "$root"/.github/workflows/release-desktop.yml; do
  if grep -q "cmd/wails@v" "$workflow"; then
    echo "check-wails-pin: $(basename "$workflow") hard-codes a CLI version; read .wails-version instead" >&2
    exit 1
  fi
done

echo "check-wails-pin: CLI and library both pinned at $pin"

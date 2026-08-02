#!/usr/bin/env bash
# Validate the exact remote main-v2 candidate, then push the three immutable
# Stable tags atomically. The v* tag activates the protected Stable relay.
set -euo pipefail

if [ "$#" -ne 1 ] || [[ ! "$1" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
	echo "usage: scripts/release-stable.sh MAJOR.MINOR.PATCH" >&2
	exit 2
fi

version="$1"
remote="${RELEASE_REMOTE:-origin}"
repository="${RELEASE_REPOSITORY:-esengine/DeepSeek-Reasonix}"
wait_seconds="${RELEASE_CI_WAIT_SECONDS:-1800}"
poll_seconds="${RELEASE_CI_POLL_SECONDS:-10}"
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

if [[ ! "$wait_seconds" =~ ^[0-9]+$ ]] || [[ ! "$poll_seconds" =~ ^[1-9][0-9]*$ ]]; then
	echo "RELEASE_CI_WAIT_SECONDS must be non-negative and RELEASE_CI_POLL_SECONDS must be positive" >&2
	exit 2
fi
for command in git gh jq node; do
	command -v "$command" >/dev/null || {
		echo "required command is unavailable: $command" >&2
		exit 2
	}
done

candidate="$(git ls-remote --heads "$remote" refs/heads/main-v2 | awk 'NR == 1 { print $1 }')"
if [[ ! "$candidate" =~ ^[0-9a-f]{40}$ ]]; then
	echo "cannot resolve $remote/main-v2" >&2
	exit 1
fi
git fetch --quiet --no-tags "$remote" refs/heads/main-v2

catalog="$(mktemp "${TMPDIR:-/tmp}/reasonix-stable-catalog.XXXXXX")"
cleanup() {
	case "$catalog" in
	*/reasonix-stable-catalog.*) rm -f -- "$catalog" ;;
	*) echo "refusing to clean unexpected catalog path: $catalog" >&2 ;;
	esac
}
trap cleanup EXIT
git show "$candidate:release-notes/releases.json" >"$catalog"
node "$script_dir/validate-stable-release-record.mjs" "$catalog" "$version"

tags=("v$version" "npm-v$version" "desktop-v$version")
for tag in "${tags[@]}"; do
	if git ls-remote --exit-code --tags --refs "$remote" "refs/tags/$tag" >/dev/null 2>&1; then
		echo "release tag already exists and will not be moved: $tag" >&2
		exit 1
	fi
done

deadline=$((SECONDS + wait_seconds))
while :; do
	runs="$(gh run list --repo "$repository" --workflow ci.yml --commit "$candidate" \
		--event push --limit 20 --json headSha,status,conclusion 2>/dev/null || true)"
	run_status="$(jq -r --arg sha "$candidate" \
		'[.[] | select(.headSha == $sha)][0].status // ""' <<<"$runs" 2>/dev/null || true)"
	conclusion="$(jq -r --arg sha "$candidate" \
		'[.[] | select(.headSha == $sha and .status == "completed")][0].conclusion // ""' <<<"$runs" 2>/dev/null || true)"
	case "$conclusion" in
	success) break ;;
	failure | cancelled | timed_out | action_required | startup_failure)
		echo "CI for $candidate concluded $conclusion" >&2
		exit 1
		;;
	esac
	if [ "$SECONDS" -ge "$deadline" ]; then
		echo "timed out waiting for successful main-v2 CI on $candidate (last status: ${run_status:-missing})" >&2
		exit 1
	fi
	sleep "$poll_seconds"
done

git push --atomic "$remote" \
	"$candidate:refs/tags/${tags[0]}" \
	"$candidate:refs/tags/${tags[1]}" \
	"$candidate:refs/tags/${tags[2]}"

for tag in "${tags[@]}"; do
	remote_sha="$(git ls-remote --tags --refs "$remote" "refs/tags/$tag" | awk 'NR == 1 { print $1 }')"
	if [ "$remote_sha" != "$candidate" ]; then
		echo "$tag resolved to ${remote_sha:-missing}; expected $candidate" >&2
		exit 1
	fi
done

echo "Stable tags pushed atomically at $candidate: ${tags[*]}"
echo "Release stable will request one release-environment approval."

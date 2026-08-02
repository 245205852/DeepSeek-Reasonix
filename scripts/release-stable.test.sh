#!/usr/bin/env bash
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
release_script="$repo_root/scripts/release-stable.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/reasonix-stable-tag-test.XXXXXX")"
cleanup() {
	case "$test_root" in
	*/reasonix-stable-tag-test.*) rm -rf -- "$test_root" ;;
	*) echo "refusing to clean unexpected test directory: $test_root" >&2 ;;
	esac
}
trap cleanup EXIT

mkdir -p "$test_root/bin"
cat >"$test_root/bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [ "${FAKE_GH_CONCLUSION:-success}" = pending ]; then
	printf '[{"headSha":"%s","status":"in_progress","conclusion":""}]\n' "$FAKE_CANDIDATE"
else
	printf '[{"headSha":"%s","status":"completed","conclusion":"%s"}]\n' \
		"$FAKE_CANDIDATE" "${FAKE_GH_CONCLUSION:-success}"
fi
EOF
chmod +x "$test_root/bin/gh"

make_remote() {
	local name="$1"
	local work="$test_root/$name-work"
	local remote="$test_root/$name.git"
	git init -q --bare "$remote"
	git init -q -b main-v2 "$work"
	git -C "$work" config user.name test
	git -C "$work" config user.email test@example.invalid
	mkdir -p "$work/release-notes"
	cp "$repo_root/release-notes/releases.json" "$work/release-notes/releases.json"
	git -C "$work" add release-notes/releases.json
	git -C "$work" commit -q -m candidate
	git -C "$work" remote add origin "$remote"
	git -C "$work" push -q origin main-v2
	printf '%s\n' "$work"
}

success_work="$(make_remote success)"
success_sha="$(git -C "$success_work" rev-parse HEAD)"
(
	cd "$success_work"
	PATH="$test_root/bin:$PATH" FAKE_CANDIDATE="$success_sha" \
		RELEASE_CI_WAIT_SECONDS=0 RELEASE_REMOTE=origin \
		"$release_script" 1.19.2
)
for tag in v1.19.2 npm-v1.19.2 desktop-v1.19.2; do
	[ "$(git ls-remote --tags --refs "$test_root/success.git" "refs/tags/$tag" | awk 'NR == 1 { print $1 }')" = "$success_sha" ]
done

failure_work="$(make_remote failure)"
failure_sha="$(git -C "$failure_work" rev-parse HEAD)"
if (
	cd "$failure_work"
	PATH="$test_root/bin:$PATH" FAKE_CANDIDATE="$failure_sha" FAKE_GH_CONCLUSION=failure \
		RELEASE_CI_WAIT_SECONDS=0 RELEASE_REMOTE=origin \
		"$release_script" 1.19.2
); then
	echo "failed CI unexpectedly created release tags" >&2
	exit 1
fi
[ -z "$(git ls-remote --tags --refs "$test_root/failure.git" 'refs/tags/*')" ]

occupied_work="$(make_remote occupied)"
occupied_sha="$(git -C "$occupied_work" rev-parse HEAD)"
git -C "$occupied_work" tag v1.19.2
git -C "$occupied_work" push -q origin v1.19.2
if (
	cd "$occupied_work"
	PATH="$test_root/bin:$PATH" FAKE_CANDIDATE="$occupied_sha" \
		RELEASE_CI_WAIT_SECONDS=0 RELEASE_REMOTE=origin \
		"$release_script" 1.19.2
); then
	echo "occupied release identity unexpectedly passed" >&2
	exit 1
fi
[ -z "$(git ls-remote --tags --refs "$test_root/occupied.git" refs/tags/npm-v1.19.2 refs/tags/desktop-v1.19.2)" ]

if "$release_script" 1.19.2-preview.1 >/dev/null 2>&1; then
	echo "Preview version unexpectedly passed the Stable tag helper" >&2
	exit 1
fi

echo "stable release tag helper tests: PASS"

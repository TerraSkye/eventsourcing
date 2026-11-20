#!/usr/bin/env bash
# release.sh cuts a release of the root module and of the nested modules
# that require it. See RELEASING.md for the order of the steps.
set -euo pipefail

repo=github.com/terraskye/eventsourcing
# Nested modules tagged as <dir>/vX.Y.Z alongside the root.
tagged=(sqlite postgres kurrentdb telemetry)
# Nested modules that require the root but are not tagged.
untagged=(examples/todolist)

usage() {
	cat >&2 <<EOF
usage: scripts/release.sh <command> [vX.Y.Z]

  prepare vX.Y.Z      open the release PR: version.go and the changelog heading
  tag-root vX.Y.Z     tag the merged release PR as vX.Y.Z and push the tag
  bump vX.Y.Z         open the PR that makes every nested module require vX.Y.Z
  tag-modules vX.Y.Z  check the merged bump PR and tag every nested module
  check               build, vet and test every nested module with GOWORK=off
EOF
	exit 2
}

die() {
	echo "release: $*" >&2
	exit 1
}

cd "$(git rev-parse --show-toplevel)"

need_version() {
	[[ ${1:-} =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "want a version like v1.2.3, got '${1:-}'"
}

has_tag() {
	git rev-parse -q --verify "refs/tags/$1" >/dev/null
}

# branch_from_master switches to a new branch $1 made from a fresh
# origin/master.
branch_from_master() {
	[[ -z $(git status --porcelain --untracked-files=no) ]] || die "the working tree has uncommitted changes"
	git fetch -q origin master --tags
	git switch -q -c "$1" origin/master
}

# requires reports whether origin/master's $1/go.mod requires the root at
# version $2.
requires() {
	git show "origin/master:$1/go.mod" |
		awk -v repo="$repo" -v v="$2" '($1 == repo && $2 == v) || ($1 == "require" && $2 == repo && $3 == v) { found = 1 } END { exit !found }'
}

# check builds, vets and tests every nested module against the root version
# its go.mod requires, as a user outside this repository gets it. The
# integration tests and benchmarks are vetted, not run.
check() {
	local dir failed=()
	for dir in "${tagged[@]}" "${untagged[@]}"; do
		echo "== $dir"
		if ! (cd "$dir" &&
			GOWORK=off go build ./... &&
			GOWORK=off go vet -tags=integration,bench ./... &&
			GOWORK=off go test ./...); then
			failed+=("$dir")
		fi
	done
	((${#failed[@]} == 0)) || die "do not build against the root version they require: ${failed[*]}"
}

prepare() {
	local v=$1 heading
	has_tag "$v" && die "tag $v already exists"
	heading="## [${v#v}] - $(date +%Y-%m-%d)"
	branch_from_master "release-$v"

	sed "s/InstrumentationVersion = \"v[^\"]*\"/InstrumentationVersion = \"$v\"/" version.go >version.go.tmp
	mv version.go.tmp version.go
	# The release's heading goes under [Unreleased], which stays, empty,
	# for the changes after it.
	awk -v h="$heading" '{ print } /^## \[Unreleased\]$/ && !done { print ""; print h; done = 1 }' CHANGELOG.md >CHANGELOG.md.tmp
	mv CHANGELOG.md.tmp CHANGELOG.md
	grep -qxF "$heading" CHANGELOG.md || die "CHANGELOG.md has no '## [Unreleased]' heading"

	git commit -q -am "Release $v"
	git push -q -u origin HEAD
	gh pr create --base master --title "Release $v" \
		--body "Dates the ${v#v} section of the CHANGELOG and sets \`InstrumentationVersion\` to \`$v\`. Once it is merged, run \`scripts/release.sh tag-root $v\`."
}

tag_root() {
	local v=$1
	git fetch -q origin master --tags
	has_tag "$v" && die "tag $v already exists"
	git show origin/master:version.go | grep -qF "InstrumentationVersion = \"$v\"" ||
		die "origin/master's version.go is not $v: merge the release PR first"
	git tag -a "$v" -m "$v" origin/master
	git push -q origin "refs/tags/$v"
	echo "tagged $v at $(git rev-parse --short "$v^{commit}")"
}

bump() {
	local v=$1 dir files=()
	git fetch -q origin master --tags
	has_tag "$v" || die "tag $v does not exist: run tag-root first"
	branch_from_master "require-$v"
	for dir in "${tagged[@]}" "${untagged[@]}"; do
		(cd "$dir" && GOWORK=off go get "$repo@$v" && GOWORK=off go mod tidy)
		files+=("$dir/go.mod" "$dir/go.sum")
	done
	check

	git add -- "${files[@]}"
	git commit -q -m "Require eventsourcing $v in the nested modules"
	git push -q -u origin HEAD
	gh pr create --base master --title "Require eventsourcing $v in the nested modules" \
		--body "Points ${tagged[*]} and ${untagged[*]} at the released $v, so their own tags resolve without the workspace. Each module builds, vets and tests with \`GOWORK=off\`. Once it is merged, run \`scripts/release.sh tag-modules $v\`."
}

tag_modules() {
	local v=$1 dir tag tmp tags=()
	git fetch -q origin master --tags
	for dir in "${tagged[@]}" "${untagged[@]}"; do
		requires "$dir" "$v" || die "origin/master's $dir/go.mod does not require $repo $v: merge the bump PR first"
	done
	for dir in "${tagged[@]}"; do
		has_tag "$dir/$v" && die "tag $dir/$v already exists"
		tags+=("$dir/$v")
	done

	# Check the commit being tagged, not the working tree. check runs as a
	# function in a subshell inside that worktree, so the commit does not
	# need to contain this script. The trap expands $tmp now: it runs at
	# exit, after this function's locals are gone.
	tmp=$(mktemp -d)
	trap "git worktree remove --force '$tmp'" EXIT
	git worktree add -q --detach "$tmp" origin/master
	(cd "$tmp" && check)

	for tag in "${tags[@]}"; do
		git tag -a "$tag" -m "$tag" origin/master
	done
	git push -q origin "${tags[@]/#/refs/tags/}"
	echo "tagged ${tags[*]} at $(git rev-parse --short origin/master)"
}

case ${1:-} in
prepare | tag-root | bump | tag-modules)
	need_version "${2:-}"
	cmd=${1//-/_}
	"$cmd" "$2"
	;;
check) check ;;
*) usage ;;
esac

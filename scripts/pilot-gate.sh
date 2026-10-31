#!/bin/sh
set -eu

new=$1
shift
if [ "$*" = --no-pilot ]; then
	echo "install: no pilot, as asked"
elif [ $# -gt 0 ]; then
	echo "install: unknown arguments: $*; the one there is: --no-pilot" >&2
	exit 2
elif [ -z "${GRAFT_PILOT:-}" ]; then
	echo "install: no pilot: set GRAFT_PILOT in .env to a project whose gate the new graft passes first, or install -- --no-pilot" >&2
	exit 1
else
	echo "install: the gate of the pilot $GRAFT_PILOT with the new graft"
	tmp=$(mktemp -d "${TMPDIR:-/tmp}/graft-pilot.XXXXXX")
	worktree=$tmp/pilot
	cleanup() {
		git -C "$GRAFT_PILOT" worktree remove --force "$worktree" >/dev/null 2>&1 || true
		rm -rf "$tmp"
	}
	trap cleanup EXIT
	trap 'exit 1' HUP INT TERM
	if ! git -C "$GRAFT_PILOT" worktree add --quiet --detach "$worktree" HEAD; then
		echo "install: refused: cannot make a worktree of the pilot $GRAFT_PILOT at HEAD; ~/bin/graft is left as it was" >&2
		exit 1
	fi
	if [ -f "$GRAFT_PILOT/.env" ]; then
		cp "$GRAFT_PILOT/.env" "$worktree/.env"
	fi
	gowork=$(cd "$GRAFT_PILOT" && go env GOWORK)
	if [ -n "$gowork" ] && [ "$gowork" != off ]; then
		export GOWORK=$gowork
	fi
	if ! (cd "$worktree" && PATH="$new:$PATH" "$new/graft" gate); then
		echo "install: refused: the gate of the pilot $GRAFT_PILOT failed with the new graft; ~/bin/graft is left as it was (install -- --no-pilot skips the pilot)" >&2
		exit 1
	fi
fi

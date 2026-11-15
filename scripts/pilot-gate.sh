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
	if installed_dir=$("$new/graft" tools install-dir); then
		:
	else
		install_dir_status=$?
		echo "install: refused: could not get the installed graft directory (exit $install_dir_status); ~/bin/graft is left as it was" >&2
		exit 1
	fi
	if [ -z "$installed_dir" ]; then
		echo "install: refused: could not get the installed graft directory: tools install-dir returned an empty path" >&2
		exit 1
	fi
	echo "install: the gate of the pilot $GRAFT_PILOT with the new graft"
	tmp=$(mktemp -d "${TMPDIR:-/tmp}/graft-pilot.XXXXXX")
	new_worktree=$tmp/pilot-new
	old_worktree=$tmp/pilot-installed
	cleanup() {
		git -C "$GRAFT_PILOT" worktree remove --force "$new_worktree" >/dev/null 2>&1 || true
		git -C "$GRAFT_PILOT" worktree remove --force "$old_worktree" >/dev/null 2>&1 || true
		rm -rf "$tmp"
	}
	trap cleanup EXIT
	trap 'exit 1' HUP INT TERM
	if ! rev=$(git -C "$GRAFT_PILOT" rev-parse --verify 'HEAD^{commit}'); then
		echo "install: refused: cannot resolve the pilot $GRAFT_PILOT HEAD; ~/bin/graft is left as it was" >&2
		exit 1
	fi
	if ! git -C "$GRAFT_PILOT" worktree add --quiet --detach "$new_worktree" "$rev"; then
		echo "install: refused: cannot make a worktree of the pilot $GRAFT_PILOT at $rev; ~/bin/graft is left as it was" >&2
		exit 1
	fi
	if [ -f "$GRAFT_PILOT/.env" ]; then
		cp "$GRAFT_PILOT/.env" "$new_worktree/.env"
	fi
	gowork=$(cd "$GRAFT_PILOT" && go env GOWORK)
	if [ -n "$gowork" ] && [ "$gowork" != off ]; then
		export GOWORK=$gowork
	fi
	if (cd "$new_worktree" && PATH="$new:$PATH" "$new/graft" gate); then
		:
	else
		installed_graft=$installed_dir/graft
		if [ ! -x "$installed_graft" ]; then
			echo "install: refused: the gate of the pilot $GRAFT_PILOT failed with the new graft; ~/bin/graft is left as it was (install -- --no-pilot skips the pilot); the installed graft could not be run for comparison: not found at $installed_graft" >&2
			exit 1
		fi
		if [ "$installed_graft" -ef "$new/graft" ]; then
			echo "install: refused: the gate of the pilot $GRAFT_PILOT failed with the new graft; ~/bin/graft is left as it was (install -- --no-pilot skips the pilot); the installed graft could not be run for comparison: $installed_graft is the new graft" >&2
			exit 1
		fi
		if ! git -C "$GRAFT_PILOT" worktree add --quiet --detach "$old_worktree" "$rev"; then
			echo "install: refused: the gate of the pilot $GRAFT_PILOT failed with the new graft; ~/bin/graft is left as it was (install -- --no-pilot skips the pilot); the installed graft could not be run for comparison: cannot make a fresh worktree at $rev" >&2
			exit 1
		fi
		if [ -f "$GRAFT_PILOT/.env" ]; then
			cp "$GRAFT_PILOT/.env" "$old_worktree/.env"
		fi
		echo "install: the gate of the pilot $GRAFT_PILOT with the installed graft ($installed_graft)"
		if (cd "$old_worktree" && PATH="$installed_dir:$PATH" "$installed_graft" gate); then
			echo "install: refused: the gate of the pilot $GRAFT_PILOT failed with the new graft; ~/bin/graft is left as it was (install -- --no-pilot skips the pilot); the installed graft passes it: a regression of this graft" >&2
		else
			old_exit=$?
			case $old_exit in
				125|126|127)
					echo "install: refused: the gate of the pilot $GRAFT_PILOT failed with the new graft; ~/bin/graft is left as it was (install -- --no-pilot skips the pilot); the installed graft could not be run for comparison: gate command exited $old_exit" >&2
					exit 1
					;;
			esac
			echo "install: refused: the gate of the pilot $GRAFT_PILOT failed with the new graft; ~/bin/graft is left as it was (install -- --no-pilot skips the pilot); the installed graft fails the pilot's gate too: likely the pilot's own failure; fix the pilot, or install -- --no-pilot" >&2
		fi
		exit 1
	fi
fi

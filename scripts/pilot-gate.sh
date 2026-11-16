#!/bin/sh
set -eu

new=$1
shift
gate_pid=
gate_starting=0
gate_cancelled=0
signal_received=0
cleaning=0
tmp=
new_worktree=
old_worktree=

cleanup() {
	cleaning=1
	if [ -n "$new_worktree" ]; then
		git -C "$GRAFT_PILOT" worktree remove --force "$new_worktree" >/dev/null 2>&1 || true
	fi
	if [ -n "$old_worktree" ]; then
		git -C "$GRAFT_PILOT" worktree remove --force "$old_worktree" >/dev/null 2>&1 || true
	fi
	if [ -n "$tmp" ]; then
		rm -rf "$tmp"
		tmp=
	fi
	cleaning=0
	if [ "$signal_received" -eq 1 ]; then
		exit 1
	fi
}

stop_gate() {
	pid=$gate_pid
	case $pid in
		''|*[!0-9]*) return ;;
	esac
	if command -v pstop >/dev/null 2>&1; then
		if pstop --grace 1s "$pid"; then
			wait "$pid" 2>/dev/null || true
			gate_pid=
			return
		fi
	fi
	# pstop ships with akit and is absent on hosts without it. setsid gives
	# this gate its own process group, so the fallback can stop its descendants.
	# setsid makes this PID the process-group ID. The PID fallback handles a
	# signal that lands before setsid has established its new session.
	kill -TERM -- "-$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true
	# Leave TERM a short chance to run cleanup, then bound cancellation even
	# when the gate ignores it. Both signals target only the process we started
	# and its dedicated process group.
	sleep 1
	kill -KILL -- "-$pid" 2>/dev/null || kill -KILL "$pid" 2>/dev/null || true
	wait "$pid" 2>/dev/null || true
	gate_pid=
}

run_gate() {
	if [ "$signal_received" -eq 1 ]; then
		exit 1
	fi
	gate_cancelled=0
	gate_starting=1
	# Background jobs otherwise get /dev/null on stdin in a non-interactive shell.
	setsid "$@" 0<&0 & gate_pid=$!
	gate_starting=0
	if [ "$gate_cancelled" -eq 1 ]; then
		stop_gate
		exit 1
	fi
	if wait "$gate_pid"; then
		gate_status=0
	else
		gate_status=$?
	fi
	gate_pid=
	return "$gate_status"
}

on_signal() {
	signal_received=1
	if [ "$gate_starting" -eq 1 ] && [ -z "$gate_pid" ]; then
		# Defer cancellation until the just-started PID has been captured.
		gate_cancelled=1
		return
	fi
	if [ -z "$gate_pid" ]; then
		if [ "$cleaning" -eq 1 ]; then
			# Finish removing both worktrees and the temporary directory.
			return
		fi
		exit 1
	fi
	trap '' HUP INT TERM
	stop_gate
	exit 1
}

trap cleanup EXIT
trap on_signal HUP INT TERM

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
	if ! command -v setsid >/dev/null 2>&1; then
		echo "install: refused: setsid is required to stop the pilot gate and its descendants on cancellation" >&2
		exit 1
	fi
	echo "install: the gate of the pilot $GRAFT_PILOT with the new graft"
	tmp=$(mktemp -d "${TMPDIR:-/tmp}/graft-pilot.XXXXXX")
	new_worktree=$tmp/pilot-new
	old_worktree=$tmp/pilot-installed
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
	if run_gate sh -c 'cd "$1" && PATH="$2:$PATH" exec "$3" gate' sh "$new_worktree" "$new" "$new/graft"; then
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
		if run_gate sh -c 'cd "$1" && PATH="$2:$PATH" exec "$3" gate' sh "$old_worktree" "$installed_dir" "$installed_graft"; then
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

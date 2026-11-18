#!/usr/bin/env bash
# Start the Glasshouse GNOME desktop VM, and keep its data across restarts.
#
#   ./start.sh [project-dir]   first run: create a desktop session that keeps its disk; later runs: boot it again
#   ./start.sh stop            shut the VM down (the disk is kept, ./start.sh boots it again)
#   ./start.sh status          show the saved session
#   ./start.sh reset           delete the saved session and ALL data inside it (asks first)
#
# What survives a stop, a restart of the VM and even a reboot of this machine: everything inside the VM (installed
# software, files, the Claude login). What does not: a rebuilt base image (image/build.sh) makes the saved disk
# unusable, because it is layered on the old image; pull your work first (glasshouse pull) before rebuilding.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GH="${GLASSHOUSE_BIN:-$HERE/bin/glasshouse}"
DATA="${GLASSHOUSE_HOME:-$HOME/.local/share/glasshouse}"
ID_FILE="$DATA/desktop-session"

die() { echo "start.sh: $*" >&2; exit 1; }

# the binary must exist and know `start` (older builds do not)
if [ ! -x "$GH" ] || ! "$GH" help 2>&1 | grep -q 'glasshouse start'; then
  [ "$GH" = "$HERE/bin/glasshouse" ] || die "$GH is missing or too old (no 'start' command)"
  echo "==> building glasshouse"
  make -C "$HERE" build >/dev/null
fi
# macOS: the VM runs in the signed Virtualization.framework helper next to the binary (or $GLASSHOUSE_VZ)
if [ "$(uname -s)" = Darwin ] && [ -z "${GLASSHOUSE_VZ:-}" ] && [ ! -x "$(dirname "$GH")/glasshouse-vz" ]; then
  [ "$GH" = "$HERE/bin/glasshouse" ] || die "$(dirname "$GH")/glasshouse-vz is missing; build it with: make -C $HERE vz-helper"
  echo "==> building the Virtualization.framework helper"
  make -C "$HERE" vz-helper >/dev/null
fi

saved_id() { if [ -s "$ID_FILE" ]; then tr -d '[:space:]' < "$ID_FILE"; fi; }
# state of a session from `glasshouse ls`; empty when it no longer exists
state_of() { "$GH" ls 2>/dev/null | awk -v id="$1" '$1 == id { print $2; exit }'; }

cmd="${1:-start}"
case "$cmd" in
  status)
    id="$(saved_id)"
    [ -n "$id" ] || { echo "no saved desktop session"; exit 0; }
    st="$(state_of "$id")"
    echo "session $id: ${st:-gone}"
    ;;

  stop)
    id="$(saved_id)"; [ -n "$id" ] || die "no saved desktop session"
    "$GH" stop "$id"
    ;;

  reset)
    id="$(saved_id)"; [ -n "$id" ] || die "no saved desktop session"
    read -r -p "Delete session $id and everything inside it? Type the session id to confirm: " ans
    [ "$ans" = "$id" ] || die "not confirmed, nothing deleted"
    [ "$(state_of "$id")" = "ready" ] && "$GH" stop "$id"
    "$GH" rm "$id" && rm -f "$ID_FILE"
    ;;

  start|*)
    # `./start.sh ./proj` : the argument is a project directory, only used when a new session is created
    project=""; [ "$cmd" != start ] && project="$cmd"
    id="$(saved_id)"
    if [ -n "$id" ]; then
      case "$(state_of "$id")" in
        ready)
          echo "session $id is already running; opening the desktop"
          exec "$GH" gui "$id" ;;
        stopped|failed|dead)
          echo "==> booting session $id again (data kept)"
          exec "$GH" start "$id" ;;
        "")
          echo "saved session $id no longer exists; creating a new one" >&2
          rm -f "$ID_FILE" ;;
        *)
          die "session $id is in state '$(state_of "$id")'; wait a moment, or see: $GH logs $id" ;;
      esac
    fi

    echo "==> creating a desktop session that keeps its disk"
    log="$(mktemp)"
    # Progress is shown by following a file, not through a pipe: the VM's processes outlive this script and would keep
    # a pipe open forever, so the script would never return.
    tail -n +1 -f "$log" >&2 & follower=$!
    trap 'kill "$follower" 2>/dev/null || true; rm -f "$log"' EXIT
    args=(run --gui --keep)
    [ -n "$project" ] && args+=("$project")
    set +e
    "$GH" "${args[@]}" 2> "$log"
    rc=$?
    set -e
    sleep 0.3
    # the session id is printed as soon as the session exists, also when the boot then fails
    new="$(grep -o -m1 's-[0-9a-f]\{6\}' "$log" || true)"
    if [ -n "$new" ]; then mkdir -p "$DATA"; echo "$new" > "$ID_FILE"; echo "saved session $new in $ID_FILE"; fi
    exit "$rc"
    ;;
esac

#!/bin/sh
# restore-store.sh — restore a spindle filesystem object store from a
# backup-store.sh archive into a FRESH directory.
#
#   sh scripts/restore-store.sh --from /backup/spindle-2026-11-22.tar.gz --to /var/lib/spindle/store
#
# The target must be absent or empty: restoring over a live store can leave a
# mix of old and new objects. Put the archive's contents at the configured
# store root (store.root, or <data-dir>/store by default), fix ownership for
# the service user, then start the instance against the same (or a new)
# config. See docs/BACKUP.md.
set -eu

usage() {
	cat <<'EOF'
usage: restore-store.sh --from ARCHIVE.tar.gz --to STORE_DIR

  --from FILE  archive written by backup-store.sh
  --to DIR     store root to create (must be absent or empty)

Verifies ARCHIVE.sha256 when it exists, then extracts. It does not chown.
EOF
}

from=
to=
while [ $# -gt 0 ]; do
	case "$1" in
	--from)
		[ $# -ge 2 ] || { usage >&2; exit 2; }
		from=$2
		shift 2
		;;
	--to)
		[ $# -ge 2 ] || { usage >&2; exit 2; }
		to=$2
		shift 2
		;;
	-h | --help) usage; exit 0 ;;
	*) echo "restore-store: unknown argument: $1" >&2; usage >&2; exit 2 ;;
	esac
done
if [ -z "$from" ] || [ -z "$to" ]; then
	usage >&2
	exit 2
fi
if [ ! -f "$from" ]; then
	echo "restore-store: archive not found: $from" >&2
	exit 1
fi
if [ -e "$to" ] && [ -n "$(ls -A "$to" 2>/dev/null || true)" ]; then
	echo "restore-store: target is not empty: $to (refusing to merge into a live store)" >&2
	exit 1
fi

if [ -f "$from.sha256" ]; then
	dir=$(dirname "$from")
	base=$(basename "$from")
	if command -v sha256sum >/dev/null 2>&1; then
		(cd "$dir" && sha256sum -c "$base.sha256")
	elif command -v shasum >/dev/null 2>&1; then
		(cd "$dir" && shasum -a 256 -c "$base.sha256")
	else
		echo "restore-store: need sha256sum or shasum to verify the snapshot" >&2
		exit 1
	fi
fi

mkdir -p "$to"
tar -C "$to" -xpzf "$from"

files=$(find "$to" -type f | wc -l | tr -d ' ')
echo "restore-store: $files files restored into $to"

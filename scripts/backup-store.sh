#!/bin/sh
# backup-store.sh — snapshot a spindle filesystem object store.
#
#   sh scripts/backup-store.sh --from /var/lib/spindle/store --to /backup/spindle-$(date -I).tar.gz
#
# The store directory is the repository (docs/BACKUP.md): the archive is a
# complete, restorable backup of every git repo, identity record, issue, pull
# request, package tag and Threads run. The local cache and the serving copies
# are NOT in the store and are not backed up.
#
# This script does not quiesce anything. Stop the process first (SIGTERM) so no
# push is mid-flight; a live copy can catch a log segment/pack that no manifest
# names yet (harmless) or a manifest whose objects were not copied (corrupt).
# See docs/BACKUP.md.
set -eu

usage() {
	cat <<'EOF'
usage: backup-store.sh --from STORE_DIR --to ARCHIVE.tar.gz

  --from DIR   the store root (store.root, or <data-dir>/store by default)
  --to FILE    archive to write (parent directories are created)

Writes ARCHIVE and ARCHIVE.sha256. Run it against a quiesced store.
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
	*) echo "backup-store: unknown argument: $1" >&2; usage >&2; exit 2 ;;
	esac
done
if [ -z "$from" ] || [ -z "$to" ]; then
	usage >&2
	exit 2
fi
if [ ! -d "$from" ]; then
	echo "backup-store: not a directory: $from" >&2
	exit 1
fi

sha256_file() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		echo "backup-store: need sha256sum or shasum" >&2
		exit 1
	fi
}

mkdir -p "$(dirname "$to")"
tmp="$to.tmp.$$"
trap 'rm -f "$tmp"' EXIT INT TERM

# Copy the directory contents (not the directory name), preserving modes and
# mtimes: the filesystem backend's CAS token is "<size>:<mtime_ns>", and the
# store's own read path never depends on wall-clock age, but preserving the
# mtimes keeps the snapshot byte-for-byte faithful to what the instance wrote.
tar -C "$from" -cpzf "$tmp" .
mv "$tmp" "$to"
trap - EXIT INT TERM

sum=$(sha256_file "$to")
printf '%s  %s\n' "$sum" "$(basename "$to")" >"$to.sha256"

files=$(find "$from" -type f | wc -l | tr -d ' ')
bytes=$(wc -c <"$to" | tr -d ' ')
echo "backup-store: $files files -> $to ($bytes bytes, sha256 $sum)"

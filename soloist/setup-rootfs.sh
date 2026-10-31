#!/system/bin/sh
# One-time: stop the old librespot receiver and unpack the Debian rootfs.
# Run as: mysudo sh -c 'sh /path/setup-rootfs.sh'
BB=${BUSYBOX:-/vendor/xbin/busybox}
BASE=${SOLOIST_BASE:-${1:-/data/local/tmp/soloist}}
ARCHIVE=${ROOTFS_ARCHIVE:-$BASE/rootfs.tar.xz}

# stop librespot pipeline (loop script first so it does not respawn)
for p in $(ps -A -o PID,ARGS | grep spotify-connect.sh | grep -v grep | awk '{print $1}'); do kill "$p"; done
killall librespot aaudio_pipe 2>/dev/null || true
sleep 1
ps -A | grep -E "librespot|aaudio_pipe" && echo "WARN: still running" || true

mkdir -p "$BASE/rootfs"
cd "$BASE/rootfs" || exit 1
$BB xzcat "$ARCHIVE" | $BB tar -x -f - || exit 1
rm "$ARCHIVE"
ls "$BASE/rootfs"
du -sh "$BASE/rootfs"

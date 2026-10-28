#!/system/bin/sh
# One-time: stop the old librespot receiver and unpack the Debian rootfs.
BB=/vendor/xbin/busybox
BASE=/data/local/tmp/soloist

# stop librespot pipeline (loop script first so it does not respawn)
for p in $(ps -A -o PID,ARGS | grep spotify-connect.sh | grep -v grep | awk '{print $1}'); do kill $p; done
killall librespot aaudio_pipe 2>/dev/null
sleep 1
ps -A | grep -E "librespot|aaudio_pipe" && echo "WARN: still running"

mkdir -p $BASE/rootfs
cd $BASE/rootfs || exit 1
$BB xzcat $BASE/rootfs.tar.xz | $BB tar -x -f - || exit 1
rm $BASE/rootfs.tar.xz
ls $BASE/rootfs
du -sh $BASE/rootfs

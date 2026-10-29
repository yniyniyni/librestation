#!/bin/sh
# Builds voiced + mic.dex (+ negatives.bin if missing) and pushes them to the station.
# Also pushes the supervisor scripts (soloist-service.sh, stop.sh) that run voiced.
# Restart the service afterwards:
#   adb shell -tt 'mysudo -n sh /data/local/tmp/soloist/stop.sh'
#   adb shell -tt 'mysudo -n sh /data/local/tmp/soloist/start.sh'
set -eu
here=$(cd "$(dirname "$0")" && pwd)
dev=${ADB_DEVICE:-speaker-ip:5555}
py=${NEG_PYTHON:-python3}
dst=/data/local/tmp/voice

(cd "$here" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o dist/voiced ./cmd/voiced)
sh "$here/mic/build.sh"
if [ ! -s "$here/dist/negatives.bin" ]; then
    "$py" -I "$here/tools/build_negatives.py" "$here/dist/negatives.bin"
fi

adb -s "$dev" shell "mkdir -p $dst/data"
for f in dist/voiced dist/mic.dex dist/negatives.bin mic/mic.sh mic/hwvol.sh; do
    adb -s "$dev" push "$here/$f" "$dst/$(basename "$f")"
done
adb -s "$dev" shell "chmod 755 $dst/voiced; ls -l $dst"

sdst=/data/local/tmp/soloist
for f in soloist-service.sh stop.sh; do
    adb -s "$dev" push "$here/../soloist/$f" "$sdst/$f"
done
adb -s "$dev" shell "chmod 755 $sdst/soloist-service.sh $sdst/stop.sh"

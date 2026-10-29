#!/bin/sh
# Builds voiced + mic.dex (+ negatives.bin if missing) and pushes them to the station.
# Also pushes the supervisor scripts (soloist-service.sh, stop.sh) that run voiced,
# aaudio_pipe and the PulseAudio config (the float sink aaudio_pipe expects).
# Restart the service afterwards:
#   adb shell -tt 'mysudo -n sh /data/local/tmp/soloist/stop.sh'
#   adb shell -tt 'mysudo -n sh /data/local/tmp/soloist/start.sh'
set -eu
here=$(cd "$(dirname "$0")" && pwd)
dev=${ADB_DEVICE:-speaker-ip:5555}
py=${NEG_PYTHON:-python3}
dst=/data/local/tmp/voice

(cd "$here" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o dist/voiced ./cmd/voiced)
(cd "$here" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o dist/ledd ./cmd/ledd)
sh "$here/mic/build.sh"
ndk=${NDK:-$(ls -d "$HOME"/Library/Android/sdk/ndk/* | tail -1)}
"$ndk"/toolchains/llvm/prebuilt/*/bin/aarch64-linux-android28-clang -O2 -Wall \
    -o "$here/../aaudio-pipe/aaudio_pipe" "$here/../aaudio-pipe/aaudio_pipe.c" -laaudio
if [ ! -s "$here/dist/negatives.bin" ]; then
    "$py" -I "$here/tools/build_negatives.py" "$here/dist/negatives.bin"
fi

adb -s "$dev" shell "mkdir -p $dst/data"
for f in dist/voiced dist/ledd dist/mic.dex dist/negatives.bin mic/mic.sh mic/hwvol.sh; do
    adb -s "$dev" push "$here/$f" "$dst/$(basename "$f")"
done
adb -s "$dev" shell "chmod 755 $dst/voiced $dst/ledd; ls -l $dst"

# stock LED assets (25x16 frames, fonts) live in the io.sdk app data, readable only by root.
# Copy them once into $dst/assets; the copy is best-effort and never fails the deploy.
cat > "$here/dist/ledd-assets.sh" <<EOF
#!/system/bin/sh
src=/data/data/com.yandex.io.sdk/files/system/frontal_animation
dst=$dst/assets
if [ -d "\$src" ]; then
    mkdir -p "\$dst" && cp -r "\$src/25x16" "\$src/fonts" "\$dst/" && chmod -R a+rX "\$dst"
    echo "ledd assets copied to \$dst"
else
    echo "ledd assets: \$src is missing, keeping existing \$dst"
fi
EOF
{ adb -s "$dev" push "$here/dist/ledd-assets.sh" /data/local/tmp/ledd-assets.sh \
    && adb -s "$dev" shell -tt "mysudo -n sh /data/local/tmp/ledd-assets.sh"; } \
    || echo "warning: ledd asset copy failed, keeping existing assets"

sdst=/data/local/tmp/soloist
for f in soloist-service.sh stop.sh; do
    adb -s "$dev" push "$here/../soloist/$f" "$sdst/$f"
done
adb -s "$dev" push "$here/../aaudio-pipe/aaudio_pipe" "$sdst/aaudio_pipe"
adb -s "$dev" push "$here/../soloist/soloist.pa" "$sdst/soloist.pa"
adb -s "$dev" shell "chmod 755 $sdst/soloist-service.sh $sdst/stop.sh $sdst/aaudio_pipe"

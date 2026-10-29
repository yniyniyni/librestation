#!/bin/sh
# Build the station binaries and push them to a rooted Yandex Station.
# Remote commands that need root are always run through mysudo su shell.
set -eu

here=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
dev=${ADB_DEVICE:?set ADB_DEVICE to the speaker, e.g. ADB_DEVICE=192.168.1.50:5555}
py=${NEG_PYTHON:-python3}
sdk=${ANDROID_SDK:-}
base=${REMOTE_BASE:-/data/local/tmp}
voice_dir=${REMOTE_VOICE_DIR:-$base/voice}
soloist_dir=${REMOTE_SOLOIST_DIR:-$base/soloist}
case "$base$voice_dir$soloist_dir" in *[!A-Za-z0-9_./-]*) echo "remote paths may contain only letters, digits, _, ., / and -" >&2; exit 1 ;; esac

if [ -z "$sdk" ] && [ "$(uname -s)" = Darwin ]; then
    sdk=$HOME/Library/Android/sdk
fi
if [ -z "$sdk" ]; then
    echo "ANDROID_SDK is required (or use the macOS default $HOME/Library/Android/sdk)" >&2
    exit 1
fi

adb_cmd() { adb -s "$dev" "$@"; }
root_shell() {
    # Call only with commands made from trusted local variables; no user input is
    # interpolated into the command string except validated absolute paths.
    adb_cmd shell -tt "mysudo su shell -c \"$*\""
}

need_cmd() {
    command -v "$1" >/dev/null 2>&1 || { echo "missing host command: $1" >&2; exit 1; }
}
need_cmd adb
need_cmd go
need_cmd javac
need_cmd "$py"

(cd "$here" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o dist/voiced ./cmd/voiced)
(cd "$here" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o dist/ledd ./cmd/ledd)
ANDROID_SDK="$sdk" sh "$here/mic/build.sh"

ndk=${NDK:-}
if [ -z "$ndk" ]; then
    for candidate in "$sdk"/ndk/*; do
        [ -d "$candidate" ] && ndk=$candidate
    done
fi
if [ -z "$ndk" ] || [ ! -d "$ndk" ]; then
    echo "NDK is required; set NDK to an Android NDK directory" >&2
    exit 1
fi
clang=$(find "$ndk/toolchains/llvm/prebuilt" -type f -name aarch64-linux-android28-clang -print -quit)
[ -n "$clang" ] || { echo "aarch64-linux-android28-clang not found under $ndk" >&2; exit 1; }
"$clang" -O2 -Wall \
    -o "$here/../aaudio-pipe/aaudio_pipe" "$here/../aaudio-pipe/aaudio_pipe.c" -laaudio
if [ ! -s "$here/dist/negatives.bin" ]; then
    "$py" -I "$here/tools/build_negatives.py" "$here/dist/negatives.bin"
fi

root_shell "mkdir -p $voice_dir/data $soloist_dir"
for f in dist/voiced dist/ledd dist/mic.dex dist/negatives.bin mic/mic.sh mic/hwvol.sh; do
    adb_cmd push "$here/$f" "$voice_dir/$(basename "$f")"
done
adb_cmd push "$here/../soloist/soloist-service.sh" "$soloist_dir/soloist-service.sh"
adb_cmd push "$here/../soloist/setup-rootfs.sh" "$soloist_dir/setup-rootfs.sh"
adb_cmd push "$here/../soloist/chroot.sh" "$soloist_dir/chroot.sh"
adb_cmd push "$here/../soloist/start.sh" "$soloist_dir/start.sh"
adb_cmd push "$here/../soloist/stop.sh" "$soloist_dir/stop.sh"
adb_cmd push "$here/../soloist/soloist.pa" "$soloist_dir/soloist.pa"
adb_cmd push "$here/../aaudio-pipe/aaudio_pipe" "$soloist_dir/aaudio_pipe"
root_shell "chmod 755 $voice_dir/voiced $voice_dir/ledd $voice_dir/mic.sh $voice_dir/hwvol.sh $soloist_dir/soloist-service.sh $soloist_dir/setup-rootfs.sh $soloist_dir/chroot.sh $soloist_dir/start.sh $soloist_dir/stop.sh $soloist_dir/aaudio_pipe"

# Stock LED assets (25x16 frames, fonts) are readable only as root.
cat > "$here/dist/ledd-assets.sh" <<EOF2
#!/system/bin/sh
src=${YASDK_ASSETS_DIR:-/data/data/com.yandex.io.sdk/files/system/frontal_animation}
dst=$voice_dir/assets
if [ -d "\$src" ]; then
    mkdir -p "\$dst" && cp -r "\$src/25x16" "\$src/fonts" "\$dst/" && chmod -R a+rX "\$dst"
    echo "ledd assets copied to \$dst"
else
    echo "ledd assets: \$src is missing, keeping existing \$dst"
fi
EOF2
adb_cmd push "$here/dist/ledd-assets.sh" "$base/ledd-assets.sh"
root_shell "sh $base/ledd-assets.sh" || echo "warning: ledd asset copy failed, keeping existing assets"

root_shell "ls -l $voice_dir $soloist_dir"
printf '%s\n' "deployed to $dev: voice=$voice_dir soloist=$soloist_dir"

#!/bin/sh
# Build the station binaries and push them to a rooted Yandex Station.
# Remote commands that need root go through mysudo, which needs a tty and
# splits its arguments on spaces (`mysudo sh -c "a b"` runs `sh -c a`), so each
# one is written to a script on the speaker and run as `mysudo -n sh <script>`.
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
    # mysudo always exits 0, so the script reports its own status.
    script=$base/.librestation-root.sh
    printf '(\nset -e\n%s\n)\necho "__rc=$?"\n' "$*" | adb_cmd shell "cat > $script"
    out=$(adb_cmd shell -tt "mysudo -n sh $script" | tr -d '\r')
    adb_cmd shell "rm -f $script"
    printf '%s\n' "$out" | grep -v '^__rc=' || true
    printf '%s\n' "$out" | grep -q '^__rc=0$'
}

need_cmd() {
    command -v "$1" >/dev/null 2>&1 || { echo "missing host command: $1" >&2; exit 1; }
}
need_cmd adb
need_cmd go
need_cmd javac
need_cmd "$py"

# -buildvcs=false: under sudo git refuses a repo owned by another user
# ("dubious ownership") and go build fails with "error obtaining VCS status".
echo "building voiced, ledd"
(cd "$here" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -buildvcs=false -o dist/voiced ./cmd/voiced)
(cd "$here" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -buildvcs=false -o dist/ledd ./cmd/ledd)
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
echo "building aaudio_pipe"
"$clang" -O2 -Wall \
    -o "$here/../aaudio-pipe/aaudio_pipe" "$here/../aaudio-pipe/aaudio_pipe.c" -laaudio
if [ ! -s "$here/dist/negatives.bin" ]; then
    echo "downloading negatives.bin (~25 MB from huggingface.co, once)"
    "$py" -I "$here/tools/build_negatives.py" "$here/dist/negatives.bin"
fi

echo "pushing to $dev"
# adb push runs as the shell user, but the target dirs are created (and may
# already hold files) as root, so push into a shell-owned staging dir and let
# root move everything into place. mv swaps the inode, so a running voiced
# does not block the update.
stage=$base/.librestation-stage
root_shell "rm -rf $stage"
adb_cmd shell "mkdir -p $stage/voice $stage/soloist $stage/sounds"

# javaenv.sh: mysudo starts mic.sh/hwvol.sh with almost no environment, but
# app_process needs these from the normal adb shell. A valid file already on
# the speaker is kept (it may be hand-made with more variables); set
# REGEN_JAVAENV=1 to rebuild it anyway.
javaenv_vars="BOOTCLASSPATH ANDROID_ROOT ANDROID_DATA"
javaenv_ok=1
if [ "${REGEN_JAVAENV:-0}" != 1 ]; then
    root_shell "f=$soloist_dir/javaenv.sh
[ -s \$f ]
unset $javaenv_vars
. \$f
[ -n \"\$BOOTCLASSPATH\" ] && [ -n \"\$ANDROID_ROOT\" ] && [ -n \"\$ANDROID_DATA\" ]" >/dev/null && javaenv_ok=0
fi
if [ "$javaenv_ok" = 0 ]; then
    echo "keeping $soloist_dir/javaenv.sh (REGEN_JAVAENV=1 rebuilds it)"
else
    echo "generating javaenv.sh"
    javaenv=$here/dist/javaenv.sh
    adb_cmd shell 'printf "%s\n" "$BOOTCLASSPATH" "$ANDROID_ROOT" "$ANDROID_DATA"' | tr -d '\r' > "$javaenv.raw"
    [ "$(wc -l < "$javaenv.raw")" -eq 3 ] || { echo "error: unexpected adb shell output:" >&2; cat "$javaenv.raw" >&2; exit 1; }
    : > "$javaenv"
    n=0
    for v in $javaenv_vars; do
        n=$((n + 1))
        val=$(sed -n "${n}p" "$javaenv.raw")
        case "$val" in
        ''|*[!A-Za-z0-9_./:+-]*) echo "error: bad $v in the speaker's adb shell: '$val'" >&2; exit 1 ;;
        esac
        printf 'export %s="%s"\n' "$v" "$val" >> "$javaenv"
    done
    rm -f "$javaenv.raw"
    adb_cmd push "$javaenv" "$stage/soloist/"
fi

for f in voice/dist/voiced voice/dist/ledd voice/dist/mic.dex voice/dist/negatives.bin \
    voice/mic/mic.sh voice/mic/hwvol.sh; do
    adb_cmd push "$here/../$f" "$stage/voice/"
done
# Stock phrases voiced plays when the network or Gemini fails. The stock
# files are MP3 despite the .wav name; voiced reads only PCM WAV, so convert
# copies to 16-bit mono 24 kHz WAV (backup/ itself is never touched).
sounds_tmp=$(mktemp -d)
trap 'rm -rf "$sounds_tmp"' EXIT
for f in no_internet something_went_wrong; do
    src=$here/../backup/sounds/$f.wav
    out=$sounds_tmp/$f.wav
    if [ ! -s "$src" ]; then
        echo "warning: backup/sounds/$f.wav is missing (backup/ is not in git), that stock phrase will not play" >&2
        continue
    fi
    if command -v ffmpeg >/dev/null 2>&1; then
        ffmpeg -loglevel error -y -i "$src" -ac 1 -ar 24000 -sample_fmt s16 "$out" || rm -f "$out"
    elif command -v afconvert >/dev/null 2>&1; then
        afconvert -f WAVE -d LEI16@24000 -c 1 "$src" "$out" || rm -f "$out"
    else
        cp "$src" "$out"
    fi
    if [ ! -s "$out" ] || [ "$(head -c 4 "$out")" != RIFF ]; then
        echo "warning: backup/sounds/$f.wav is not a WAV and could not be converted (install ffmpeg), skipping it; voiced will say the phrase through TTS" >&2
        continue
    fi
    adb_cmd push "$out" "$stage/sounds/"
done
for f in soloist/soloist-service.sh soloist/setup-rootfs.sh soloist/chroot.sh soloist/start.sh \
    soloist/stop.sh soloist/soloist.pa aaudio-pipe/aaudio_pipe; do
    adb_cmd push "$here/../$f" "$stage/soloist/"
done
root_shell "mkdir -p $voice_dir/data $voice_dir/sounds $soloist_dir
for f in $stage/voice/*; do mv -f \"\$f\" $voice_dir/; done
for f in $stage/sounds/*; do if [ -f \"\$f\" ]; then mv -f \"\$f\" $voice_dir/sounds/; fi; done
for f in $stage/soloist/*; do mv -f \"\$f\" $soloist_dir/; done
rm -rf $stage"
root_shell "chmod 755 $voice_dir/voiced $voice_dir/ledd $voice_dir/mic.sh $voice_dir/hwvol.sh $soloist_dir/soloist-service.sh $soloist_dir/setup-rootfs.sh $soloist_dir/chroot.sh $soloist_dir/start.sh $soloist_dir/stop.sh $soloist_dir/aaudio_pipe
chmod 644 $soloist_dir/javaenv.sh"

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
adb_cmd shell "rm -f $base/ledd-assets.sh"

root_shell "ls -l $voice_dir $soloist_dir"
printf '%s\n' "deployed to $dev: voice=$voice_dir soloist=$soloist_dir"

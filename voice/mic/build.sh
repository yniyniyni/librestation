#!/bin/sh
# Builds dist/mic.dex (Mic, HwVol) against the Android 28 SDK.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
sdk=${ANDROID_SDK:-$HOME/Library/Android/sdk}
out=$here/../dist
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$out"
javac -source 8 -target 8 -bootclasspath "$sdk/platforms/android-28/android.jar" -d "$tmp" "$here/Mic.java" "$here/HwVol.java"
"$sdk/build-tools/34.0.0/d8" --min-api 28 --output "$tmp" "$tmp"/*.class
cp "$tmp/classes.dex" "$out/mic.dex"
echo "built $out/mic.dex"

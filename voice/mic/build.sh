#!/bin/sh
# Builds mic.dex (Mic, HwVol) against the Android SDK.
set -eu
here=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
sdk=${ANDROID_SDK:-}
if [ -z "$sdk" ] && [ "$(uname -s)" = Darwin ]; then
    sdk=$HOME/Library/Android/sdk
fi
[ -n "$sdk" ] || { echo "ANDROID_SDK is required (or use the macOS default $HOME/Library/Android/sdk)" >&2; exit 1; }
platform=${ANDROID_PLATFORM:-android-28}
build_tools=${ANDROID_BUILD_TOOLS_VERSION:-34.0.0}
out=${BUILD_OUTPUT_DIR:-$here/../dist}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
[ -f "$sdk/platforms/$platform/android.jar" ] || { echo "missing Android platform: $sdk/platforms/$platform" >&2; exit 1; }
[ -x "$sdk/build-tools/$build_tools/d8" ] || { echo "missing d8: $sdk/build-tools/$build_tools/d8" >&2; exit 1; }
mkdir -p "$out"
javac -source 8 -target 8 -bootclasspath "$sdk/platforms/$platform/android.jar" -d "$tmp" "$here/Mic.java" "$here/HwVol.java"
"$sdk/build-tools/$build_tools/d8" --min-api 28 --output "$tmp" "$tmp"/*.class
cp "$tmp/classes.dex" "$out/mic.dex"
echo "built $out/mic.dex"

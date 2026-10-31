#!/bin/sh
# Ask for the Soloist API key without echoing it and store it on the speaker.
set -e
: ${ADB_DEVICE:?'set ADB_DEVICE to the speaker, e.g. ADB_DEVICE=192.168.1.50:5555'}
base=${REMOTE_BASE:-/data/local/tmp}
soloist_dir=${REMOTE_SOLOIST_DIR:-$base/soloist}
case "$base$soloist_dir" in /*) ;; *) echo "remote paths must be absolute" >&2; exit 1 ;; esac
case "$base$soloist_dir" in *[!A-Za-z0-9_./-]*) echo "remote paths may contain only letters, digits, _, ., / and -" >&2; exit 1 ;; esac
printf "Soloist API key: "
stty -echo
IFS= read -r KEY
stty echo
printf "\n"
if [ -z "$KEY" ]; then
    echo "empty key, nothing written" >&2
    exit 1
fi
# The key is sent over stdin, never interpolated into the remote shell command.
printf '%s' "$KEY" | adb -s "$ADB_DEVICE" shell -tt "mysudo sh -c 'mkdir -p "$soloist_dir" && cat > "$soloist_dir/api-key" && chmod 600 "$soloist_dir/api-key"'"
unset KEY
echo "ok, key saved on speaker"

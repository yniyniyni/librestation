#!/bin/sh
# Ask for a Gemini API key without echoing it and store it on the speaker,
# where voiced uses it to label hits. Get a key at https://aistudio.google.com/apikey
set -e
: ${ADB_DEVICE:?'set ADB_DEVICE to the speaker, e.g. ADB_DEVICE=192.168.1.50:5555'}
voice_dir=${REMOTE_VOICE_DIR:-/data/local/tmp/voice}
case "$voice_dir" in /*) ;; *) echo "remote paths must be absolute" >&2; exit 1 ;; esac
case "$voice_dir" in *[!A-Za-z0-9_./-]*) echo "remote paths may contain only letters, digits, _, ., / and -" >&2; exit 1 ;; esac
printf "Gemini API key: "
stty -echo
IFS= read -r KEY
stty echo
printf "\n"
if [ -z "$KEY" ]; then
    echo "empty key, nothing written" >&2
    exit 1
fi
# The key goes over stdin (no tty, so no echo), never into the remote command.
# The adb shell user owns the voice dir, and voiced runs as root, so no root
# is needed here.
printf '%s' "$KEY" | adb -s "$ADB_DEVICE" shell "umask 077 && cat > $voice_dir/gemini.key"
unset KEY
echo "ok, key saved on speaker; restart voiced to use it"

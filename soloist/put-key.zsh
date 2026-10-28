#!/bin/zsh
# Asks for the Soloist API key without echoing it and stores it on the speaker.
read -rs "KEY?Soloist API key: "
echo
if [[ -z $KEY ]]; then
    echo "empty key, nothing written"
    exit 1
fi
printf '%s' "$KEY" | adb shell 'cat > /data/local/tmp/soloist/api-key' && echo "ok, key saved on speaker"
unset KEY

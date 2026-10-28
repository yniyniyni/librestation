#!/system/bin/sh
# Spotify Connect receiver: librespot -> stdout PCM -> aaudio_pipe -> audioserver.
# Run as root; restarts the pipeline if it dies.

DIR=/data/local/tmp/spotify-connect
LOG=$DIR/spotify-connect.log
NAME="Яндекс Станция"

# wait for network and audioserver after boot
until getprop init.svc.audioserver | grep -q running && ip -4 addr show wlan0 | grep -q inet; do
    sleep 5
done

while true; do
    echo "--- start $(date)" >> $LOG
    "$DIR/librespot" \
        --name "$NAME" \
        --device-type speaker \
        --backend pipe \
        --bitrate 320 \
        --initial-volume 50 \
        --cache "$DIR/cache" \
        --cache-size-limit 200M \
        2>> $LOG | "$DIR/aaudio_pipe" 44100 2>> $LOG
    sleep 5
    # keep the log small
    [ "$(wc -c < $LOG)" -gt 1000000 ] && tail -c 200000 $LOG > $LOG.tmp && mv $LOG.tmp $LOG
done

#!/system/bin/sh
# Spotify Soloist receiver on Android:
#   soloist (Debian chroot) -> PulseAudio pipe-sink -> FIFO -> aaudio_pipe -> audioserver.
# Run as root. Each part is restarted on its own if it dies.

BB=/vendor/xbin/busybox
S=/data/local/tmp/soloist
R=$S/rootfs
LOG=$S/soloist.log
FIFO=$R/tmp/pulse-out.fifo
NAME="Яндекс Станция"

log() { echo "--- $* $(date)" >> $LOG; }

# keep one pipeline alive, logging to $LOG; $1 = label, rest = command
supervise() {
    label=$1; shift
    while true; do
        log "start $label"
        "$@" >> $LOG 2>&1
        log "exit $label rc=$?"
        sleep 3
    done
}

until getprop init.svc.audioserver | grep -q running && ip -4 addr show wlan0 | grep -q inet; do
    sleep 5
done

if [ ! -s $S/api-key ]; then
    log "no API key in $S/api-key, not starting"
    exit 1
fi
mkdir -p $R/etc/soloist
cp $S/api-key $R/etc/soloist/api-key
chmod 600 $S/api-key $R/etc/soloist/api-key

# the PulseAudio config is deployed next to this script (the rootfs is root-only)
[ -s $S/soloist.pa ] && cp $S/soloist.pa $R/etc/pulse/soloist.pa

[ -p $FIFO ] || { rm -f $FIFO; mkfifo $FIFO; }
# a pid file left by a power cut makes pulseaudio refuse to start
rm -f $R/root/.config/pulse/*-runtime/pid

# trim the log on start; it only grows by restarts and warnings
[ -f $LOG ] && [ "$(wc -c < $LOG)" -gt 1000000 ] && tail -c 200000 $LOG > $LOG.tmp && mv $LOG.tmp $LOG

# opening the FIFO read-write keeps aaudio_pipe from seeing EOF when pulseaudio restarts
# voiced writes $S/gain: the inverse of Soloist's software volume (see voice/internal/volume)
supervise aaudio_pipe sh -c "exec $S/aaudio_pipe 44100 $S/gain <> $FIFO" &

supervise pulseaudio sh $S/chroot.sh pulseaudio -n -F /etc/pulse/soloist.pa \
    --exit-idle-time=-1 --daemonize=no --log-target=stderr --log-level=error &

# Spotify's login hosts are blocked from this network, so Soloist goes through a local
# Xray proxy when $S/xray.json exists (socks on 127.0.0.1:1080, http on 127.0.0.1:1081)
PROXY_ENV=
if [ -s $S/xray.json ]; then
    cp $S/xray.json $R/etc/xray.json
    chmod 600 $S/xray.json $R/etc/xray.json
    supervise xray sh $S/chroot.sh /opt/xray/xray run -c /etc/xray.json &
    PROXY_ENV="HTTPS_PROXY=http://127.0.0.1:1081 HTTP_PROXY=http://127.0.0.1:1081 ALL_PROXY=socks5h://127.0.0.1:1080 NO_PROXY=localhost,127.0.0.1"
fi

sleep 3
# extra flags (e.g. --verbose) from $S/flags; chroot.sh clears the environment, so pass them inline
SOLOIST_FLAGS=$(cat $S/flags 2>/dev/null)
supervise soloist sh $S/chroot.sh /bin/sh -c \
    'export PULSE_SERVER=unix:/tmp/pulse.sock $2; exec /opt/soloist/soloist \
        --device-name "$0" --api-key "$(cat /etc/soloist/api-key)" \
        --data-dir /var/lib/soloist --cache-dir /var/cache/soloist --cache-size 300 \
        --initial-volume 15 --ws 127.0.0.1:9771 $1' "$NAME" "$SOLOIST_FLAGS" "$PROXY_ENV" &

# LED daemon: clock, volume ring and carousel on the front panel
supervise ledd /data/local/tmp/voice/ledd &
supervise voiced /data/local/tmp/voice/voiced run &

wait

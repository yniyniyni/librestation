#!/system/bin/sh
# Stop soloist-service.sh and everything it started.
BB=/vendor/xbin/busybox
PAT='soloist-service.sh|/opt/soloist/soloist|soloist --|pulseaudio -n -F /etc/pulse/soloist.pa|aaudio_pipe 44100|xray run -c /etc/xray.json|voice-bridge.sh|logcat -v raw -T 1 -s quark-native'
# supervisors first so nothing respawns, then the rest
for pass in 1 2; do
    for p in $(ps -A -o PID,ARGS | grep -E "$PAT" | grep -v -E "grep|stop.sh" | $BB awk '{print $1}'); do
        $BB kill -9 $p 2>/dev/null
    done
    sleep 1
done
rm -f /data/local/tmp/soloist/rootfs/root/.config/pulse/*-runtime/pid 2>/dev/null
ps -A -o PID,ARGS | grep -E "$PAT" | grep -v -E "grep|stop.sh" || echo stopped

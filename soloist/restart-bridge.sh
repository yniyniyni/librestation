#!/system/bin/sh
# restart only the voice bridge; its supervisor respawns it in 3 s
for p in $(ps -A -o PID,ARGS | grep -E "voice-bridge.sh$|logcat -v raw -T 1 -s quark-native" | grep -v grep | /vendor/xbin/busybox awk '{print $1}'); do
    case $(cat /proc/$p/cmdline | tr '\0' ' ') in *"sh /data/local/tmp/soloist/voice-bridge.sh"*|*logcat*) /vendor/xbin/busybox kill -9 $p ;; esac
done
sleep 5
ps -A -o PID,ARGS | grep -E "voice-bridge|logcat -v raw" | grep -v grep

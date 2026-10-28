#!/system/bin/sh
# Device launcher: streams 16ch/48k raw PCM to stdout. Needs root (use mysudo).
. /data/local/tmp/soloist/javaenv.sh
export CLASSPATH=/data/local/tmp/voice/mic.dex
exec app_process /system/bin Mic

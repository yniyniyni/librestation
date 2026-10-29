#!/system/bin/sh
# Device launcher: sets the hardware volume, $1 = 0..100 or - (read values from stdin). Needs root (use mysudo).
. /data/local/tmp/soloist/javaenv.sh
export CLASSPATH=/data/local/tmp/voice/mic.dex
exec app_process /system/bin HwVol "$@"

#!/system/bin/sh
# Device launcher: pins the hardware volume, $1 = 0..100. Needs root (use mysudo).
. /data/local/tmp/soloist/javaenv.sh
export CLASSPATH=/data/local/tmp/voice/mic.dex
exec app_process /system/bin HwVol "$@"

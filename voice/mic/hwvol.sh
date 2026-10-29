#!/system/bin/sh
# Device launcher: sets the hardware volume, $1 = 0..100 or - (read values from stdin). Needs root (use mysudo).
BASE=${SOLOIST_BASE:-/data/local/tmp/soloist}
VOICE=${VOICE_DIR:-/data/local/tmp/voice}
. "$BASE/javaenv.sh"
export CLASSPATH="$VOICE/mic.dex"
exec app_process /system/bin HwVol "$@"

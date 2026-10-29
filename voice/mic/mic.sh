#!/system/bin/sh
# Device launcher: streams 16ch/48k raw PCM to stdout. Needs root (use mysudo).
BASE=${SOLOIST_BASE:-/data/local/tmp/soloist}
VOICE=${VOICE_DIR:-/data/local/tmp/voice}
. "$BASE/javaenv.sh"
export CLASSPATH="$VOICE/mic.dex"
exec app_process /system/bin Mic

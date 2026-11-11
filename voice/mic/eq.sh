#!/system/bin/sh
# Device launcher: sets the EQ mode, $1 = 0..9. Needs root (use mysudo).
BASE=${SOLOIST_BASE:-/data/local/tmp/soloist}
VOICE=${VOICE_DIR:-/data/local/tmp/voice}
. "$BASE/javaenv.sh"
export CLASSPATH="$VOICE/mic.dex"
exec app_process /system/bin EqMode "$@"

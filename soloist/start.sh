#!/system/bin/sh
# Start soloist-service.sh detached from the calling shell.
BB=${BUSYBOX:-/vendor/xbin/busybox}
BASE=${SOLOIST_BASE:-/data/local/tmp/soloist}
$BB setsid sh "$BASE/soloist-service.sh" </dev/null >/dev/null 2>&1 &

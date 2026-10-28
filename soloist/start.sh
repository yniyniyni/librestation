#!/system/bin/sh
# Start soloist-service.sh detached from the calling shell.
/vendor/xbin/busybox setsid sh /data/local/tmp/soloist/soloist-service.sh </dev/null >/dev/null 2>&1 &

#!/system/bin/sh
# Put back the original Yandex error phrases saved by mute-errors.sh.
S=/data/local/tmp/soloist
SND=/data/data/com.yandex.io.sdk/files/system/sounds/ru-RU
for f in $S/sounds-orig/*; do cat $f > $SND/$(basename $f); done
ls -la $SND | grep -E "backend_unavailable|no_internet|vins_error|something_went_wrong|auth_failed"

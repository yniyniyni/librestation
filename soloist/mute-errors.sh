#!/system/bin/sh
# Replace Yandex's "no network" phrases with silence (backend is cut off on purpose).
# Originals are kept in $S/sounds-orig; unmute-errors.sh puts them back.
S=/data/local/tmp/soloist
SND=/data/data/com.yandex.io.sdk/files/system/sounds/ru-RU
mkdir -p $S/sounds-orig
for f in backend_unavailable.opus no_internet.wav vins_error.wav something_went_wrong.wav auth_failed.wav; do
    [ -f $S/sounds-orig/$f ] || cp $SND/$f $S/sounds-orig/$f
    case $f in *.opus) src=$S/rootfs/tmp/silence.opus ;; *) src=$S/rootfs/tmp/silence.mp3 ;; esac
    cat $src > $SND/$f   # keeps owner and SELinux label
done
ls -la $SND | grep -E "backend_unavailable|no_internet|vins_error|something_went_wrong|auth_failed"

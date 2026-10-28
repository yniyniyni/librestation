#!/system/bin/sh
# Run a command inside the Debian chroot, mounting what it needs first (idempotent).
# Usage: chroot.sh <command> [args...]
BB=/vendor/xbin/busybox
R=/data/local/tmp/soloist/rootfs

mounted() { $BB grep -q " $1 " /proc/mounts; }

mounted $R/proc || $BB mount -t proc proc $R/proc
mounted $R/sys || $BB mount -t sysfs sysfs $R/sys
mounted $R/dev || $BB mount -o bind /dev $R/dev
mounted $R/dev/pts || $BB mount -t devpts devpts $R/dev/pts
mounted $R/dev/shm || { $BB mkdir -p $R/dev/shm; $BB mount -t tmpfs -o mode=1777 tmpfs $R/dev/shm; }

# Android's DNS server is not in a resolv.conf; take it from props
rm -f $R/etc/resolv.conf  # stock image links it to systemd-resolved
echo "nameserver $(getprop net.dns1)" > $R/etc/resolv.conf
grep -q localhost $R/etc/hosts 2>/dev/null || echo "127.0.0.1 localhost" > $R/etc/hosts

exec $BB chroot $R /usr/bin/env -i \
    HOME=/root TERM=dumb LANG=C.UTF-8 \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    "$@"

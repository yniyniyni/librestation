#!/system/bin/sh
# LED test frames for the Station Max (run as root on the device).
#   fbtest.sh ring I0 I1 I2   light ring pixels I0 red, I1 green, I2 blue (dim), rest off
#   fbtest.sh screen          screen: left half gray 255, right half gray 32
#   fbtest.sh off             ring and screen black
# Frames are raw RGB: fb5 = 24 pixels (72 bytes), fb4 = 25x16 (1200 bytes).
RING=/dev/graphics/fb5
SCREEN=/dev/graphics/fb4
T=/data/local/tmp/fbtest.raw

pixels() { # $1 = count; prints that many black RGB pixels
    i=0
    while [ $i -lt $1 ]; do printf '\000\000\000'; i=$((i + 1)); done
}

case "$1" in
ring)
    i=0
    while [ $i -lt 24 ]; do
        if [ $i -eq "$2" ]; then printf '\100\000\000'
        elif [ $i -eq "$3" ]; then printf '\000\100\000'
        elif [ $i -eq "$4" ]; then printf '\000\000\100'
        else printf '\000\000\000'
        fi
        i=$((i + 1))
    done > $T
    cat $T > $RING
    ;;
screen)
    y=0
    while [ $y -lt 16 ]; do
        x=0
        while [ $x -lt 25 ]; do
            if [ $x -lt 12 ]; then printf '\377\377\377'; else printf '\040\040\040'; fi
            x=$((x + 1))
        done
        y=$((y + 1))
    done > $T
    cat $T > $SCREEN
    ;;
off)
    pixels 24 > $T; cat $T > $RING
    pixels 400 > $T; cat $T > $SCREEN
    ;;
*)
    echo "usage: $0 ring I0 I1 I2 | screen | off" >&2
    exit 2
    ;;
esac

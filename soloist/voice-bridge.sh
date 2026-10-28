#!/system/bin/sh
# Offline voice commands -> Soloist.
# Yandex's local command spotter logs "Command phrase spotted: <word>" to logcat even with
# its backend cut off; we map those words to soloist ctl calls over its WebSocket API.

S=/data/local/tmp/soloist
WS=127.0.0.1:9771
last=
last_t=0

# -T 1: start from the newest line, do not replay old commands
logcat -v raw -T 1 -s quark-native:I | while read -r line; do
    case "$line" in
        *"Command phrase spotted: "*) word=${line##*Command phrase spotted: } ;;
        *"onPhraseSpotted: алиса"*)
            # offline, "Алиса" only listens into a dead backend for ~5 s with the command
            # spotter off; cancel it right away so the next command is heard
            echo "voice: алиса -> cancel listening"
            (sleep 0.4; sh $S/alice-button.sh) &
            continue ;;
        *) continue ;;
    esac
    case "$word" in
        пауза|стоп|хватит) cmd=pause ;;
        продолжи|играй) cmd=play ;;
        дальше|следующий) cmd=next ;;
        назад) cmd=prev ;;
        *) continue ;;
    esac
    # the spotter often fires twice for one utterance; a double "next" would skip a track
    now=$(date +%s)
    if [ "$cmd" = "$last" ] && [ $((now - last_t)) -lt 3 ]; then
        continue
    fi
    last=$cmd
    last_t=$now
    echo "voice: $word -> $cmd"
    sh $S/chroot.sh /opt/soloist/soloist ctl -w $WS $cmd 2>&1 &
    # Yandex can't pause locally without its own player, so it sends "пауза" to its
    # (blocked) backend and says it feels unwell 15 s later. A press of the Alice
    # button while that request is busy cancels it.
    if [ $cmd = pause ]; then
        (sleep 0.5; sh $S/alice-button.sh) &
    fi
done

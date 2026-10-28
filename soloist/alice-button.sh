#!/system/bin/sh
# short press of the Alice button (KEY_VOICECOMMAND=582) on gpio_keypad
sendevent /dev/input/event0 1 582 1
sendevent /dev/input/event0 0 0 0
sendevent /dev/input/event0 1 582 0
sendevent /dev/input/event0 0 0 0

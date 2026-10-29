# Station Max LED hardware notes (device spike, 2026-10-30)

Measured on the speaker (adb speaker-ip:5555) after removing Yandex io.sdk.
Test script: `voice/tools/fbtest.sh` (push to /data/local/tmp, run as root).

## io.sdk

- `pm disable-user com.yandex.io.sdk` is refused: "Cannot disable a protected package".
- Done instead: `pm uninstall -k --user 0 com.yandex.io.sdk` (data kept, reversible with
  `cmd package install-existing com.yandex.io.sdk`). It no longer runs or respawns.
- After a reboot without io.sdk: voiced, Soloist, the mic helper and audio all start by
  themselves, mic level ok (−36 dBFS), time and timezone (Europe/Moscow) correct.
- Without io.sdk nobody sets the screen backlight: it came up at **200** (max). `ledd` must set
  it on start. Set to 36 by hand for now.
- Stock assets were copied before the uninstall to `/data/local/tmp/voice/assets/`
  (`25x16/`, `fonts/`, `ledpatterns/`, 147 files). The io.sdk data dir still exists (`-k`).

## Ring — /dev/graphics/fb5

- Plain `write()` of 72 bytes at offset 0 shows immediately. `root:graphics 0660`.
- **Byte order is BGR**: each pixel is B, G, R. (Pixel 0 = `40 00 00` lit `ring1` blue.)
- Pixel i drives sysfs `station2:*:ring<i+1>`.
- **Geometry**: pixel 0 is at the front centre (the side with the screen, nearest the user);
  increasing index goes front → right → back → left, i.e. **counter-clockwise seen from above**.
  Pixel 6 = right, 12 = back, 18 = left.
- **Decision for the code:** logical ring index = hardware index (`RingTop = 0`,
  `RingClockwise = false` in the plan's terms, i.e. no remap). "Front"/"near" = index 0 is the
  focal point: stock volume fills from it toward the right (exactly like stock `volume.led`,
  which uses hardware indexes), the "stock" AI arc grows around it, pause converges to it and
  play spreads from it; "next" sweeps toward increasing index, "prev" toward decreasing.

## Screen — /dev/graphics/fb4

- Plain `write()` of 1200 bytes (25×16 RGB) at offset 0 shows immediately.
- Gray levels work: a 255 half and a 32 half are clearly different.
- An all-black frame is fully dark: the FPGA draws nothing of its own.
- FPGA sysfs: `.../spi32766.0/hwmon/hwmon1/frames_queue/{frames_in_frame_queue,next_frame_delay,
  flush_frame_queue,frame_queue_overflows,disable_test_mode}` (queue unused by plain writes,
  stays 0), firmware `fw:15;api:9`.
- Backlight: `/sys/class/backlight/fb4/brightness`, 0..200.

## Light sensor

- `/sys/bus/iio/devices/iio:device0/` (name `ltr308als01`); `in_intensity_input` (lux-like
  float) and `in_intensity_raw`; gain 3, integration 0.1 s.
- Night readings: 1.2–1.8 (raw 6–9) with the room lamp on or off — very low and barely
  changing. No daytime reading yet: the curve must be calibrated by day (ledd logs readings).

## Soloist (ws://127.0.0.1:9771)

- `playback_state` and `playback_changed` carry `status`: seen `playing`, `paused`,
  `buffering` (track changes and seek go playing → buffering → playing within ~1 s).
- `get_state` request gets a `playback_state` reply on connect.
- Other events seen: `auth_state`, `volume_changed`, `position_sync`, `track_changed`,
  `queue_changed`, `context_changed`, `error`.

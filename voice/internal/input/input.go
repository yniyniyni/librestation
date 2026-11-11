// Package input reads key presses from a Linux evdev device such as the
// station's volume knob (/dev/input/event2, "rotary-volume").
package input

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// EventSize is sizeof(struct input_event) on arm64: a 16-byte timeval,
// then u16 type, u16 code, s32 value.
const EventSize = 24

// Event types and the knob's key codes (linux/input-event-codes.h).
const (
	EvKey       = 1
	KeyPageUp   = 104 // knob turned clockwise: louder
	KeyPageDown = 109 // knob turned counter-clockwise: quieter

	SwMuteDevice = 14 // the station's mic button (gpio_keypad, /dev/input/event0)
)

// BLE remote key codes (YNDX-00402, Vendor_057a_Product_006f.kl). The physical
// remote does not always send the scancode the key layout lists first: its OK
// sends KEY_SELECT (353), not 28, and it has dedicated media-transport keys.
// These were confirmed by logging live presses, not taken from the doc.
const (
	KeyTVInput     = 59 // F1 remapped to TV_INPUT (HDMI/AV source select)
	KeyEnter       = 28 // DPAD_CENTER (alternate to KeySelect)
	KeyDpadUp      = 103
	KeyDpadLeft    = 105
	KeyDpadRight   = 106
	KeyDpadDown    = 108
	KeyMute        = 113 // VOLUME_MUTE
	KeyVolumeDown  = 114
	KeyVolumeUp    = 115
	KeyPower       = 116
	KeyMenu        = 139
	KeyBack        = 158
	KeyMediaNext   = 163 // MEDIA_NEXT
	KeyPlayPause   = 164
	KeyMediaPrev   = 165 // MEDIA_PREVIOUS
	KeyRewind      = 168 // MEDIA_REWIND
	KeyHome        = 172
	KeyFastForward = 208 // MEDIA_FAST_FORWARD — this remote's "вперёд"
	KeyVoiceAssist = 217
	KeyPairing     = 231 // BLE pairing; no Android keycode, not forwarded
	KeySelect      = 353 // KEY_SELECT: this remote's OK button (DPAD_CENTER)
	KeyAppSwitch   = 580
	KeySearch      = 582
)

// Event is one decoded input_event without its timestamp.
type Event struct {
	Type  uint16
	Code  uint16
	Value int32
}

// Press reports a key going down; releases (0) and autorepeat (2) are not presses.
func (e Event) Press() bool { return e.Type == EvKey && e.Value == 1 }

// ParseEvent decodes a 24-byte arm64 input_event.
func ParseEvent(b []byte) (Event, bool) {
	if len(b) < EventSize {
		return Event{}, false
	}
	return Event{
		Type:  binary.LittleEndian.Uint16(b[16:]),
		Code:  binary.LittleEndian.Uint16(b[18:]),
		Value: int32(binary.LittleEndian.Uint32(b[20:])),
	}, true
}

// SwitchOn reports the current state of switch code on the evdev device at
// path. It takes no grab, so other readers keep seeing the events.
func SwitchOn(path string, code uint) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var state [8]byte
	if err := readSwitches(f, state[:]); err != nil {
		return false, fmt.Errorf("switches of %s: %w", path, err)
	}
	return switchBit(state[:], code), nil
}

// switchBit reads one switch from the kernel's little-endian bit array.
func switchBit(state []byte, code uint) bool {
	i := code / 8
	return i < uint(len(state)) && state[i]&(1<<(code%8)) != 0
}

// ReadKeys opens the evdev device at path, grabs it for exclusive access
// (EVIOCGRAB: other readers such as Yandex's io.sdk stop seeing its events)
// and calls onKey with the code of every key press until ctx is cancelled or
// the device fails. onKey runs on the reading goroutine. It always returns a
// non-nil error; the grab ends when the file is closed.
func ReadKeys(ctx context.Context, path string, onKey func(code uint16)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := grab(f); err != nil {
		return fmt.Errorf("grab %s: %w", path, err)
	}
	stop := context.AfterFunc(ctx, func() { f.Close() }) // unblocks the read
	defer stop()
	err = readKeys(ctx, f, onKey)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func readKeys(ctx context.Context, r io.Reader, onKey func(code uint16)) error {
	br := bufio.NewReaderSize(r, EventSize*64)
	buf := make([]byte, EventSize)
	for ctx.Err() == nil {
		if _, err := io.ReadFull(br, buf); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
		if e, _ := ParseEvent(buf); e.Press() {
			onKey(e.Code)
		}
	}
	return ctx.Err()
}

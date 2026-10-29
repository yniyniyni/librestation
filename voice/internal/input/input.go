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

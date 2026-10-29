package input

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
)

// event builds a 24-byte arm64 struct input_event.
func event(typ, code uint16, value int32) []byte {
	b := make([]byte, EventSize)
	binary.LittleEndian.PutUint64(b[0:], 1700000000) // tv_sec
	binary.LittleEndian.PutUint64(b[8:], 123456)     // tv_usec
	binary.LittleEndian.PutUint16(b[16:], typ)
	binary.LittleEndian.PutUint16(b[18:], code)
	binary.LittleEndian.PutUint32(b[20:], uint32(value))
	return b
}

func TestParseInputEvent(t *testing.T) {
	cases := []struct {
		typ, code uint16
		value     int32
		press     bool
	}{
		{EvKey, KeyPageUp, 1, true},
		{EvKey, KeyPageDown, 1, true},
		{EvKey, KeyPageUp, 0, false},   // release
		{EvKey, KeyPageDown, 2, false}, // autorepeat
		{0, 0, 0, false},               // EV_SYN
		{2, 8, 1, false},               // EV_REL
	}
	for _, c := range cases {
		e, ok := ParseEvent(event(c.typ, c.code, c.value))
		if !ok {
			t.Fatalf("ParseEvent(%d,%d,%d) not ok", c.typ, c.code, c.value)
		}
		if e.Type != c.typ || e.Code != c.code || e.Value != c.value {
			t.Errorf("got %+v, want type %d code %d value %d", e, c.typ, c.code, c.value)
		}
		if e.Press() != c.press {
			t.Errorf("%+v: Press() = %v, want %v", e, e.Press(), c.press)
		}
	}
	if _, ok := ParseEvent(make([]byte, 16)); ok {
		t.Error("short buffer parsed")
	}
}

func TestReadKeysReportsPressesOnly(t *testing.T) {
	var stream bytes.Buffer
	for _, e := range [][]byte{
		event(EvKey, KeyPageUp, 1), event(0, 0, 0),
		event(EvKey, KeyPageUp, 0), event(0, 0, 0),
		event(EvKey, KeyPageDown, 1), event(EvKey, KeyPageDown, 2),
		event(EvKey, KeyPageDown, 0),
	} {
		stream.Write(e)
	}
	var got []uint16
	err := readKeys(context.Background(), &stream, func(code uint16) { got = append(got, code) })
	if err == nil {
		t.Fatal("readKeys returned nil at EOF")
	}
	if len(got) != 2 || got[0] != KeyPageUp || got[1] != KeyPageDown {
		t.Fatalf("keys = %v, want [104 109]", got)
	}
}

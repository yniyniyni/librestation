package uinput

import "testing"

// The ioctl request numbers must match the kernel's uinput.h on arm64.
func TestIoctlConstants(t *testing.T) {
	cases := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"UI_SET_EVBIT", uiSetEvbit, 0x40045564},
		{"UI_SET_KEYBIT", uiSetKeybit, 0x40045565},
		{"UI_DEV_CREATE", uiDevCreate, 0x5501},
		{"UI_DEV_DESTROY", uiDevDestroy, 0x5502},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %#x, want %#x", c.name, c.got, c.want)
		}
	}
}

// struct uinput_user_dev is 1116 bytes on arm64: name[80] + input_id(8) +
// ff_effects_max(4) + four __s32[64] abs arrays (1024).
func TestUserDevLayout(t *testing.T) {
	b := userDev("Yandex Remote 2 (librestation)", 0x057A, 0x006F)
	if len(b) != 1116 {
		t.Fatalf("userDev size = %d, want 1116", len(b))
	}
	if got := string(b[:30]); got != "Yandex Remote 2 (librestation)" {
		t.Errorf("name = %q", got)
	}
	if b[30] != 0 {
		t.Errorf("name is not NUL-terminated: b[30]=%d", b[30])
	}
	// input_id at offset 80: little-endian u16 bustype, vendor, product, version.
	id := func(off int) uint16 { return uint16(b[off]) | uint16(b[off+1])<<8 }
	if got := id(80); got != 0x05 {
		t.Errorf("bustype = %#x, want 0x05 (BUS_BLUETOOTH)", got)
	}
	if got := id(82); got != 0x057A {
		t.Errorf("vendor = %#x, want 0x057A", got)
	}
	if got := id(84); got != 0x006F {
		t.Errorf("product = %#x, want 0x006F", got)
	}
	if got := id(86); got != 1 {
		t.Errorf("version = %d, want 1", got)
	}
}

// A name longer than the field is truncated with room for the NUL terminator.
func TestUserDevLongNameTruncated(t *testing.T) {
	long := make([]byte, 200)
	for i := range long {
		long[i] = 'x'
	}
	b := userDev(string(long), 1, 2)
	if b[uinputMaxName-1] != 0 {
		t.Errorf("truncated name must keep a NUL at the last byte, got %d", b[uinputMaxName-1])
	}
}

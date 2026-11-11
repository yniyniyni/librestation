// Package uinput emits key presses through a virtual /dev/uinput device so
// voiced can re-inject the BLE remote's keys it grabbed for its own use.
//
// The alternative, the `input keyevent` command, spins up app_process (~0.65 s
// per press on the station) and blocks the reader goroutine, which made the
// remote lag badly. A uinput write is a kernel operation measured in
// microseconds.
//
// The device clones the remote's USB vendor/product (0x057A/0x006F), so Android
// picks the same key layout (Vendor_057a_Product_006f.kl) and maps the
// re-emitted Linux scancodes to the very same Android keycodes the physical
// remote would have produced. Forwarding is therefore "emit the same scancode".
package uinput

import (
	"fmt"
	"os"
	"syscall"
)

// Event types and codes (linux/input-event-codes.h).
const (
	evSyn     = 0x00
	evKey     = 0x01
	synReport = 0x00
)

// ioctl request numbers for /dev/uinput, type 'U' (0x55), _IOC encoding
// dir<<30 | size<<16 | type<<8 | nr (matches internal/input's grab style).
const (
	uiSetEvbit   = 1<<30 | 4<<16 | 'U'<<8 | 100 // _IOW('U',100,int)
	uiSetKeybit  = 1<<30 | 4<<16 | 'U'<<8 | 101 // _IOW('U',101,int)
	uiDevCreate  = 'U'<<8 | 1                   // _IO('U',1)
	uiDevDestroy = 'U'<<8 | 2                   // _IO('U',2)
)

const (
	uinputMaxName = 80
	absCnt        = 0x40 // ABS_CNT
	// uinput_user_dev: name[80] + input_id(8) + ff_effects_max(4) + 4×abs[64]×4.
	userDevSize = uinputMaxName + 8 + 4 + 4*absCnt*4
	eventSize   = 24 // struct input_event on arm64 (16-byte timeval + u16+u16+s32)
)

// Device is a virtual keyboard. It is not safe for concurrent Key calls.
type Device struct {
	f *os.File
}

// New creates a uinput device named name, carrying USB ids vendor/product, that
// can emit each Linux scancode in keys. It must be closed to remove the device.
func New(name string, vendor, product uint16, keys []uint16) (*Device, error) {
	f, err := os.OpenFile("/dev/uinput", os.O_WRONLY, 0)
	if err != nil {
		return nil, err
	}
	d := &Device{f: f}
	if err := d.ioctl(uiSetEvbit, evSyn); err != nil {
		d.f.Close()
		return nil, fmt.Errorf("set EV_SYN: %w", err)
	}
	if err := d.ioctl(uiSetEvbit, evKey); err != nil {
		d.f.Close()
		return nil, fmt.Errorf("set EV_KEY: %w", err)
	}
	for _, k := range keys {
		if err := d.ioctl(uiSetKeybit, uintptr(k)); err != nil {
			d.f.Close()
			return nil, fmt.Errorf("set key %d: %w", k, err)
		}
	}
	if _, err := f.Write(userDev(name, vendor, product)); err != nil {
		d.f.Close()
		return nil, fmt.Errorf("write setup: %w", err)
	}
	if err := d.ioctl(uiDevCreate, 0); err != nil {
		d.f.Close()
		return nil, fmt.Errorf("create: %w", err)
	}
	return d, nil
}

// Key emits one full press (down then up) of scancode code.
func (d *Device) Key(code uint16) error {
	if err := d.emit(evKey, code, 1); err != nil {
		return err
	}
	if err := d.emit(evSyn, synReport, 0); err != nil {
		return err
	}
	if err := d.emit(evKey, code, 0); err != nil {
		return err
	}
	return d.emit(evSyn, synReport, 0)
}

// Close removes the virtual device.
func (d *Device) Close() error {
	d.ioctl(uiDevDestroy, 0)
	return d.f.Close()
}

func (d *Device) emit(typ, code uint16, value int32) error {
	var b [eventSize]byte
	// bytes 0..15 are the timeval; the kernel stamps it, so leave it zero.
	b[16] = byte(typ)
	b[17] = byte(typ >> 8)
	b[18] = byte(code)
	b[19] = byte(code >> 8)
	b[20] = byte(value)
	b[21] = byte(value >> 8)
	b[22] = byte(value >> 16)
	b[23] = byte(value >> 24)
	_, err := d.f.Write(b[:])
	return err
}

func (d *Device) ioctl(req, arg uintptr) error {
	rc, err := d.f.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg)
	}); err != nil {
		return err
	}
	if errno != 0 {
		return errno
	}
	return nil
}

// userDev marshals a struct uinput_user_dev with just a name and USB ids.
func userDev(name string, vendor, product uint16) []byte {
	b := make([]byte, userDevSize)
	nb := []byte(name)
	if len(nb) > uinputMaxName-1 {
		nb = nb[:uinputMaxName-1]
	}
	copy(b, nb)
	// struct input_id at offset 80: u16 bustype, vendor, product, version.
	const busBluetooth = 0x05
	put16(b[80:], busBluetooth)
	put16(b[82:], vendor)
	put16(b[84:], product)
	put16(b[86:], 1)
	// ff_effects_max and the abs arrays stay zero.
	return b
}

func put16(b []byte, v uint16) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
}

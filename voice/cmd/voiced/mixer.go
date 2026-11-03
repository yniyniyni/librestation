package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// The amplifier's level as the kernel has it, read with one ioctl: cheap enough
// to look often, and it needs neither the audio HAL nor a process per reading.

const (
	mixerDevice  = "/dev/snd/controlC0"
	ampControl   = "Tweeters DSP Playback Volume"
	ctlElemRead  = 0xC4C85512 // SNDRV_CTL_IOCTL_ELEM_READ, 64-bit struct snd_ctl_elem_value
	ctlIfaceMix  = 2          // SNDRV_CTL_ELEM_IFACE_MIXER
	ctlValueSize = 1224
	ctlNameAt    = 16
	ctlValueAt   = 72 // after the id (64 bytes), the indirect flag and padding
)

// mixerControl reads one integer control.
type mixerControl struct {
	f    *os.File
	name string
}

func openMixerControl(device, name string) (*mixerControl, error) {
	f, err := os.OpenFile(device, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	return &mixerControl{f, name}, nil
}

func (m *mixerControl) read() (int, error) {
	var v [ctlValueSize]byte
	binary.NativeEndian.PutUint32(v[4:], ctlIfaceMix)
	copy(v[ctlNameAt:ctlNameAt+43], m.name) // numid 0: found by name
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.f.Fd(), ctlElemRead, uintptr(unsafe.Pointer(&v)))
	if e != 0 {
		return 0, fmt.Errorf("mixer %q: %w", m.name, e)
	}
	return int(binary.NativeEndian.Uint64(v[ctlValueAt:])), nil
}

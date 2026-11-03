package input

import (
	"os"
	"syscall"
	"unsafe"
)

// eviocgrab is EVIOCGRAB = _IOW('E', 0x90, int).
const eviocgrab = 0x40044590

// grab takes exclusive access to the device. It goes through SyscallConn, not
// Fd, so the file stays in the runtime poller and Close unblocks a pending Read.
func grab(f *os.File) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, eviocgrab, 1)
	}); err != nil {
		return err
	}
	if errno != 0 {
		return errno
	}
	return nil
}

// readSwitches fills state with the device's switch bits (EVIOCGSW, which is
// _IOC(_IOC_READ, 'E', 0x1b, len)).
func readSwitches(f *os.File, state []byte) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	req := uintptr(2)<<30 | uintptr(len(state))<<16 | uintptr('E')<<8 | 0x1b
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(unsafe.Pointer(&state[0])))
	}); err != nil {
		return err
	}
	if errno != 0 {
		return errno
	}
	return nil
}

package input

import (
	"os"
	"syscall"
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

package proxy

import (
	"syscall"
	"unsafe"
)

func isTerminal(fd uintptr) bool {
	var state syscall.Termios
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGETA, uintptr(unsafe.Pointer(&state)))
	return err == 0
}

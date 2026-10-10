package proxy

import (
	"syscall"
	"unsafe"
)

func isTerminal(fd uintptr) bool {
	var state syscall.Termios
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&state)))
	return err == 0
}

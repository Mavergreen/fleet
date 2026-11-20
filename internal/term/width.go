//go:build darwin || linux

// Package term asks the terminal for its width.
package term

import (
	"syscall"
	"unsafe"
)

// Width returns the column count of the terminal on fd, or 0 if fd is not
// a terminal or the ioctl fails.
func Width(fd uintptr) int {
	var ws struct{ Row, Col, X, Y uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if errno != 0 {
		return 0
	}
	return int(ws.Col)
}

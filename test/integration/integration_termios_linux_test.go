//go:build integration && linux

package integration

import (
	"syscall"
	"unsafe"
)

// makeRaw configures the PTY slave in raw mode so the test observes exactly
// the bytes exchanged by the bridge (no echo, no CR/LF translation).
func makeRaw(fd int) error {
	var old syscall.Termios
	if err := ioctlTermios(fd, syscall.TCGETS, unsafe.Pointer(&old)); err != nil {
		return err
	}
	raw := old
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	return ioctlTermios(fd, syscall.TCSETS, unsafe.Pointer(&raw))
}

func ioctlTermios(fd int, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

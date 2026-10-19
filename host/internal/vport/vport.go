// Package vport creates the local virtual serial port that user programs talk
// to. On Unix it is a PTY created with openpty(3) and exposed through a stable
// symlink; on Windows it is one half of a com0com port pair opened with
// go.bug.st/serial. The two implementations are split by build tag and share
// the Port interface below.
package vport

import "io"

// Options carries the serial settings requested on the command line. They are
// used to configure a Windows COM port; on Unix the PTY line discipline is put
// into raw mode and the numbers are only used for the remote UART_CFG frame.
type Options struct {
	Baud     uint32
	DataBits uint8
	Parity   uint8 // 0 none, 1 odd, 2 even
	StopBits uint8
	Flow     uint8 // 0 none, 1 rts/cts
}

// Port is the local endpoint of the bridge: bytes read from it are user data
// that must be sent to the device, bytes written to it appear on the virtual
// serial port.
type Port interface {
	io.ReadWriteCloser
	// Name is the path the user should open (the symlink on Unix, the COM
	// port that was opened on Windows).
	Name() string
}

// LineMonitor is optionally implemented by ports that can report DTR/RTS
// modem-control changes made by the user program. Mask uses the WSP_LINE_*
// bit values. The bridge polls it periodically and mirrors changes into
// LINE_CTRL frames.
type LineMonitor interface {
	LineMask() (mask uint8, err error)
}

// Warner is optionally implemented by ports that want to surface non-fatal
// setup warnings (for example a failed chmod on the PTY slave). The bridge
// logs them to stderr after the port is created.
type Warner interface {
	Warnings() []string
}

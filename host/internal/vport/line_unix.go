//go:build darwin || linux

package vport

import (
	"golang.org/x/sys/unix"

	"github.com/wireless-serial/wserial/internal/wsp"
)

// LineMask implements LineMonitor by polling the PTY's modem status bits.
//
// User programs normally control DTR/RTS with TIOCMSET on the tty; on Linux
// the pty driver exposes those bits through TIOCMGET on the master. On macOS
// the pseudo-terminal driver may return ENOTTY, in which case the bridge
// disables line-control mirroring automatically.
func (p *unixPort) LineMask() (uint8, error) {
	v, err := unix.IoctlGetInt(int(p.master.Fd()), unix.TIOCMGET)
	if err != nil {
		return 0, err
	}
	var mask uint8
	if v&unix.TIOCM_DTR != 0 {
		mask |= wsp.LineDTR
	}
	if v&unix.TIOCM_RTS != 0 {
		mask |= wsp.LineRTS
	}
	return mask, nil
}

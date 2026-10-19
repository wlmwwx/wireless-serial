package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"golang.org/x/term"
)

// MonitorConfig configures the monitor subcommand: a terminal-attached serial
// console instead of a virtual serial port.
type MonitorConfig struct {
	Config
	// Hex renders both directions as hexadecimal instead of raw bytes.
	Hex bool
	// In is the data source (os.Stdin in the CLI).
	In io.Reader
	// Out receives device bytes (os.Stdout in the CLI).
	Out io.Writer
	// Terminal enables raw terminal mode and Ctrl+] as the exit key.
	Terminal bool
}

// ctrlBracket is the monitor's exit key: Ctrl+].
const ctrlBracket = 0x1D

// Monitor connects to the device and pipes the terminal to the serial link.
// Ctrl+] (or a signal / cancelled context) exits cleanly.
func Monitor(ctx context.Context, mcfg MonitorConfig) error {
	cfg := mcfg.Config
	cfg.normalize()
	log := cfg.Logger
	if mcfg.In == nil {
		mcfg.In = os.Stdin
	}
	if mcfg.Out == nil {
		mcfg.Out = os.Stdout
	}
	if cfg.Raw {
		// RAW mode has no keepalive by contract; do not pretend otherwise.
		cfg.PingInterval = 0
	}

	if mcfg.Terminal {
		state, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			return fmt.Errorf("设置终端原始模式失败: %w", err)
		}
		defer func() {
			_ = term.Restore(int(os.Stdin.Fd()), state)
			// Leave the shell on a fresh line after raw-mode output.
			fmt.Fprintln(os.Stderr)
		}()
	}

	tgt, err := resolveTarget(ctx, &cfg)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	mw := &monitorWriter{out: mcfg.Out, hex: mcfg.Hex}
	inCh := make(chan []byte, 64)
	inErr := make(chan error, 1)
	done := make(chan struct{})
	defer close(done)
	go monitorInput(ctx, done, cancel, mcfg, mw, inCh, inErr)

	ep := endpoint{
		write: mw.rx,
		in:    inCh,
		inErr: inErr,
	}

	log.Infof("串口监视器已启动（Ctrl+] 退出%s）", hexHint(mcfg.Hex))
	err = connectLoop(ctx, &cfg, tgt, ep, nil)
	if errors.Is(err, context.Canceled) {
		err = nil
	}
	if err != nil {
		return err
	}
	log.Infof("串口监视器已退出")
	return nil
}

func hexHint(hex bool) string {
	if hex {
		return "，十六进制显示"
	}
	return ""
}

// monitorWriter serialises writes to the terminal from the RX and TX paths.
type monitorWriter struct {
	mu  sync.Mutex
	out io.Writer
	hex bool
}

// rx renders bytes received from the device.
func (m *monitorWriter) rx(p []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.hex {
		return writeFull(m.out, p)
	}
	return m.dumpLocked("RX", p)
}

// tx renders bytes sent to the device (hex mode only).
func (m *monitorWriter) tx(p []byte) {
	if !m.hex {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.dumpLocked("TX", p)
}

// dumpLocked writes one hex block of up to 16 bytes per line.
func (m *monitorWriter) dumpLocked(prefix string, p []byte) error {
	const perLine = 16
	for off := 0; off < len(p); off += perLine {
		end := off + perLine
		if end > len(p) {
			end = len(p)
		}
		line := prefix + " | "
		if off > 0 {
			line = "   | "
		}
		for _, b := range p[off:end] {
			line += fmt.Sprintf("%02x ", b)
		}
		if _, err := fmt.Fprintln(m.out, line); err != nil {
			return err
		}
	}
	return nil
}

// monitorInput pumps the terminal/pipe into the bridge.
func monitorInput(ctx context.Context, done <-chan struct{}, cancel context.CancelFunc, mcfg MonitorConfig, mw *monitorWriter, ch chan<- []byte, errCh chan<- error) {
	buf := make([]byte, 4096)
	for {
		n, err := mcfg.In.Read(buf)
		if n > 0 {
			data := buf[:n]
			if mcfg.Terminal {
				if i := bytes.IndexByte(data, ctrlBracket); i >= 0 {
					if i > 0 {
						if !deliverMonitorInput(ctx, done, mw, ch, data[:i]) {
							cancel()
							return
						}
					}
					cancel()
					return
				}
			}
			if !deliverMonitorInput(ctx, done, mw, ch, data) {
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				// In terminal mode EOF means the tty went away; in a pipe
				// it only means "no more input", so keep displaying output.
				if mcfg.Terminal {
					cancel()
				}
				return
			}
			select {
			case errCh <- fmt.Errorf("读取标准输入失败: %w", err):
			default:
			}
			return
		}
	}
}

// deliverMonitorInput copies and forwards one chunk. It returns false when the
// monitor is shutting down.
func deliverMonitorInput(ctx context.Context, done <-chan struct{}, mw *monitorWriter, ch chan<- []byte, data []byte) bool {
	if len(data) == 0 {
		return true
	}
	mw.tx(data)
	cp := make([]byte, len(data))
	copy(cp, data)
	select {
	case ch <- cp:
		return true
	case <-done:
		return false
	case <-ctx.Done():
		return false
	}
}

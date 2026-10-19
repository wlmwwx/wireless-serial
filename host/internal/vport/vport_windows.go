//go:build windows

package vport

import (
	"fmt"

	"go.bug.st/serial"

	"github.com/wireless-serial/wserial/internal/wsp"
)

// DefaultDevice is empty on Windows: the user must pass --device explicitly
// because the COM port number depends on their com0com setup.
const DefaultDevice = ""

// DefaultDevicePath returns the platform default virtual port path.
func DefaultDevicePath() string { return DefaultDevice }

// winPort wraps one end of a com0com port pair.
type winPort struct {
	port serial.Port
	name string
}

// Open opens the COM port named by device. The user program must open the
// other half of the com0com pair; wserial never creates Windows ports itself.
func Open(device string, opts Options) (Port, error) {
	if device == "" {
		return nil, fmt.Errorf("Windows 下必须用 -d/--device 指定 com0com 的一端（例如 --device COM12，用户程序使用对端 COM11）")
	}
	baud := int(opts.Baud)
	if baud == 0 {
		// The local COM pair carries a byte stream; 115200 is a safe
		// placeholder while the remote UART keeps its current baud.
		baud = 115200
	}
	mode := &serial.Mode{
		BaudRate: baud,
		DataBits: int(opts.DataBits),
		Parity:   parityMode(opts.Parity),
		StopBits: stopBits(opts.StopBits),
		InitialStatusBits: &serial.ModemOutputBits{
			DTR: true,
			RTS: true,
		},
	}
	port, err := serial.Open(device, mode)
	if err != nil {
		return nil, fmt.Errorf("打开串口 %s 失败: %w（请确认 com0com 已创建端口对，且该端口未被其他程序占用；用户程序应打开对端）", device, err)
	}
	if err := port.SetReadTimeout(serial.NoTimeout); err != nil {
		_ = port.Close()
		return nil, fmt.Errorf("设置 %s 读超时失败: %w", device, err)
	}
	return &winPort{port: port, name: device}, nil
}

// Name returns the COM port that was opened.
func (p *winPort) Name() string { return p.name }

// Read reads bytes from the COM port.
func (p *winPort) Read(b []byte) (int, error) { return p.port.Read(b) }

// Write writes bytes to the COM port.
func (p *winPort) Write(b []byte) (int, error) { return p.port.Write(b) }

// Close closes the COM port.
func (p *winPort) Close() error { return p.port.Close() }

func parityMode(p uint8) serial.Parity {
	switch p {
	case wsp.ParityOdd:
		return serial.OddParity
	case wsp.ParityEven:
		return serial.EvenParity
	default:
		return serial.NoParity
	}
}

func stopBits(s uint8) serial.StopBits {
	if s == 2 {
		return serial.TwoStopBits
	}
	return serial.OneStopBit
}

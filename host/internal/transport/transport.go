// Package transport provides TCP dialing and the reconnect policy used when
// the WiFi link drops while the virtual serial port must stay alive.
package transport

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"
)

// Dial opens a TCP connection to host:port with an overall timeout.
func Dial(ctx context.Context, host string, port int, timeout time.Duration) (net.Conn, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	d := net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("连接 %s 失败: %w", addr, err)
	}
	return conn, nil
}

// Backoff implements the exponential reconnect delay: 1s, 2s, 4s, 8s, 10s,
// 10s, … The sequence resets after a successful handshake.
type Backoff struct {
	Min time.Duration
	Max time.Duration

	cur time.Duration
}

// NewBackoff returns a backoff starting at min and capped at max.
func NewBackoff(min, max time.Duration) *Backoff {
	if min <= 0 {
		min = time.Second
	}
	if max < min {
		max = min
	}
	return &Backoff{Min: min, Max: max}
}

// Reset restarts the sequence so the next delay is Min.
func (b *Backoff) Reset() { b.cur = 0 }

// Next returns the delay to wait before the next attempt.
func (b *Backoff) Next() time.Duration {
	if b.cur == 0 {
		b.cur = b.Min
		return b.cur
	}
	b.cur *= 2
	if b.cur > b.Max {
		b.cur = b.Max
	}
	return b.cur
}

// Wait sleeps for d or until ctx is cancelled.
func Wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

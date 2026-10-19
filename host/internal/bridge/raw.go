package bridge

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
)

// bridgeRaw runs the RAW (port 3334) path: a pure bidirectional byte stream
// with no framing, no handshake and no keepalive. It returns any unsent bytes
// so the next connection can flush them first.
func bridgeRaw(ctx context.Context, conn net.Conn, cfg *Config, ep endpoint, pending []byte) ([]byte, error) {
	stop := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, 4)
	sendDone := make(chan sendResult, 1)

	// Cancel watcher: closing the connection unblocks both directions.
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()

	// Device -> user.
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if werr := ep.write(buf[:n]); werr != nil {
					pushErr(errCh, fmt.Errorf("写入虚拟串口失败: %w", werr))
					return
				}
			}
			if err != nil {
				if err == io.EOF {
					err = fmt.Errorf("设备关闭了 RAW 连接")
				}
				pushErr(errCh, err)
				return
			}
		}
	}()

	// User -> device.
	wg.Add(1)
	go func() {
		defer wg.Done()
		p := pending
		for {
			if len(p) == 0 {
				p = nil
				select {
				case <-stop:
					sendDone <- sendResult{}
					return
				case <-ctx.Done():
					sendDone <- sendResult{}
					return
				case p = <-ep.in:
				}
			}
			consumed, werr := writeProgress(conn, p)
			if werr != nil {
				sendDone <- sendResult{pending: append([]byte(nil), p[consumed:]...), err: werr}
				pushErr(errCh, fmt.Errorf("写入 TCP 失败: %w", werr))
				return
			}
			p = nil
		}
	}()

	var runErr error
	select {
	case runErr = <-errCh:
	case runErr = <-ep.inErr:
		runErr = fmt.Errorf("读取本地串口失败: %w", runErr)
	case <-ctx.Done():
	}

	close(stop)
	_ = conn.Close()
	wg.Wait()
	res := <-sendDone
	if ctx.Err() != nil {
		return res.pending, nil
	}
	if runErr == nil {
		runErr = res.err
	}
	return res.pending, runErr
}

// writeProgress writes p and returns the number of bytes accepted before an
// error occurred.
func writeProgress(w io.Writer, p []byte) (int, error) {
	consumed := 0
	for consumed < len(p) {
		n, err := w.Write(p[consumed:])
		if n > 0 {
			consumed += n
		}
		if err != nil {
			return consumed, err
		}
		if n == 0 {
			return consumed, io.ErrShortWrite
		}
	}
	return consumed, nil
}

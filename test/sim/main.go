// Command sim is a WSP/1 device simulator: it emulates the ESP32-C3 side of
// the wireless-serial project so the host tool and the test suite can run
// without real hardware.
//
// It listens on three TCP ports (all default to an ephemeral port so tests
// never collide):
//
//	-wsp    WSP/1 frame protocol   (device side, equivalent to TCP 3333)
//	-raw    byte-transparent passthrough (equivalent to TCP 3334)
//	-hw     "target hardware" side: the test process connects here to act as
//	        the UART peer.  Bytes written to it arrive at the simulated UART RX
//	        (and are forwarded to the serial client); bytes received from it are
//	        the serial client's TX bytes.
//
// An optional -admin HTTP endpoint exposes GET /status for assertions, and
// -addrfile atomically writes the actual addresses as JSON so the integration
// tests can discover the random ports.
//
// Example:
//
//	cd test/sim && go run . -wsp 127.0.0.1:0 -raw 127.0.0.1:0 -hw 127.0.0.1:0 \
//	    -addrfile /tmp/sim.json -log
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	var (
		wspAddr   = flag.String("wsp", "127.0.0.1:0", "WSP/1 listen address (host:port, :0 = ephemeral)")
		rawAddr   = flag.String("raw", "127.0.0.1:0", "RAW passthrough listen address (host:port)")
		hwAddr    = flag.String("hw", "127.0.0.1:0", "target-hardware side listen address (host:port)")
		adminAddr = flag.String("admin", "127.0.0.1:0", "admin HTTP listen address (empty string disables it)")
		addrFile  = flag.String("addrfile", "", "write actual listen addresses as JSON to this file")
		baud      = flag.Uint("baud", 115200, "initial target UART baud rate")
		name      = flag.String("name", "wserial-sim", "simulated device name (max 16 bytes)")
		latency   = flag.Duration("latency", 0, "extra delay before forwarding target->host bytes")
		drop      = flag.Float64("drop", 0, "probability (0..1) of dropping a target->host chunk (packet loss injection)")
		verbose   = flag.Bool("log", false, "log every received/sent frame to stderr")
		helloTO   = flag.Duration("hello-timeout", 5*time.Second, "time allowed for the client HELLO before closing")
		idleTO    = flag.Duration("idle", 120*time.Second, "idle session timeout")

		showVersion = flag.Bool("version", false, "print version and exit")
	)

	flag.Parse()

	if *showVersion {
		fmt.Println("wserial-sim 0.1.0 (WSP/1 device simulator)")
		return
	}
	if *baud == 0 || *baud > 5000000 {
		fmt.Fprintf(os.Stderr, "sim: invalid -baud %d\n", *baud)
		os.Exit(2)
	}
	if *drop < 0 || *drop > 1 {
		fmt.Fprintf(os.Stderr, "sim: invalid -drop %v (must be 0..1)\n", *drop)
		os.Exit(2)
	}
	if len(*name) > 16 {
		fmt.Fprintf(os.Stderr, "sim: -name must be at most 16 bytes\n")
		os.Exit(2)
	}

	s, err := startSim(simConfig{
		WSPAddr:      *wspAddr,
		RAWAddr:      *rawAddr,
		HWAddr:       *hwAddr,
		AdminAddr:    *adminAddr,
		Baud:         uint32(*baud),
		Name:         *name,
		Latency:      *latency,
		Drop:         *drop,
		HelloTimeout: *helloTO,
		IdleTimeout:  *idleTO,
		Verbose:      *verbose,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "sim: %v\n", err)
		os.Exit(1)
	}

	if *addrFile != "" {
		if err := s.WriteAddrFile(*addrFile); err != nil {
			fmt.Fprintf(os.Stderr, "sim: %v\n", err)
			_ = s.Close()
			os.Exit(1)
		}
	}

	// Machine-readable readiness line; scripts can also poll -addrfile.
	fmt.Println(s.ReadyLine())
	_ = os.Stdout.Sync()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	_ = s.Close()
}

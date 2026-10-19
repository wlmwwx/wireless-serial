// golang.org/x/net is pinned to >= v0.42 (via the indirect requirement below)
// because the old version required by grandcat/zeroconf v1.0.0 fails to link
// on modern Go toolchains ("invalid reference to syscall.recvmsg").
module github.com/wireless-serial/wserial

go 1.25

require (
	github.com/creack/pty v1.1.24
	github.com/grandcat/zeroconf v1.0.0
	go.bug.st/serial v1.6.2
	golang.org/x/sys v0.35.0
	golang.org/x/term v0.34.0
)

require (
	github.com/cenkalti/backoff v2.2.1+incompatible // indirect
	github.com/creack/goselect v0.1.2 // indirect
	github.com/miekg/dns v1.1.27 // indirect
	golang.org/x/crypto v0.40.0 // indirect
	golang.org/x/net v0.42.0 // indirect
)

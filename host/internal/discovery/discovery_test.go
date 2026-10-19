package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/grandcat/zeroconf"
)

func testEntry() *zeroconf.ServiceEntry {
	return &zeroconf.ServiceEntry{
		ServiceRecord: *zeroconf.NewServiceRecord("wserial-A1B2C3", "_wserial._tcp", "local."),
		HostName:      "esp32c3-112233.local.",
		Port:          3333,
		Text:          []string{"proto=1", "raw=3334", "name=车间串口", "baud=921600", "mac=aa:bb:cc:dd:ee:ff"},
		AddrIPv4:      []net.IP{net.ParseIP("192.168.1.50")},
	}
}

func TestParseEntry(t *testing.T) {
	d := parseEntry(testEntry())
	if d.Name != "车间串口" {
		t.Fatalf("Name = %q", d.Name)
	}
	if d.IP != "192.168.1.50" || d.Hostname != "esp32c3-112233.local" {
		t.Fatalf("address = %q / %q", d.IP, d.Hostname)
	}
	if d.Port != 3333 || d.RAWPort != 3334 || d.Baud != 921600 || d.Proto != 1 {
		t.Fatalf("unexpected fields: %+v", d)
	}
	if d.MAC != "AA:BB:CC:DD:EE:FF" {
		t.Fatalf("MAC = %q", d.MAC)
	}
	if d.Address() != "192.168.1.50:3333" {
		t.Fatalf("Address = %q", d.Address())
	}
	if d.RawAddress() != "192.168.1.50:3334" {
		t.Fatalf("RawAddress = %q", d.RawAddress())
	}
}

func TestParseEntryFallbacks(t *testing.T) {
	e := &zeroconf.ServiceEntry{
		ServiceRecord: *zeroconf.NewServiceRecord("wserial-raw", "_wserial._tcp", "local."),
		HostName:      "fallback.local.",
		Port:          3333,
	}
	d := parseEntry(e)
	if d.Name != "wserial-raw" {
		t.Fatalf("Name fallback = %q", d.Name)
	}
	if d.IP != "" || d.Hostname != "fallback.local" {
		t.Fatalf("host = %+v", d)
	}
	if d.Address() != "fallback.local:3333" {
		t.Fatalf("Address fallback = %q", d.Address())
	}
	if d.RawAddress() != "fallback.local:3334" {
		t.Fatalf("RawAddress fallback = %q", d.RawAddress())
	}

	// IPv6-only entries must produce a bracketed address.
	e2 := &zeroconf.ServiceEntry{
		ServiceRecord: *zeroconf.NewServiceRecord("v6", "_wserial._tcp", "local."),
		Port:          3333,
		AddrIPv6:      []net.IP{net.ParseIP("fe80::1")},
	}
	d2 := parseEntry(e2)
	if !strings.HasPrefix(d2.Address(), "[") {
		t.Fatalf("IPv6 Address = %q", d2.Address())
	}
}

func TestFormatTable(t *testing.T) {
	out := FormatTable([]Device{
		{Name: "dev1", IP: "192.168.1.50", Port: 3333, MAC: "AA:BB:CC:DD:EE:FF", Baud: 115200},
		{Name: "dev2", IP: "192.168.1.51", Port: 3333},
	})
	for _, want := range []string{"名称", "IP", "端口", "MAC", "波特率", "dev1", "192.168.1.50", "3333", "115200", "-"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table missing %q:\n%s", want, out)
		}
	}
}

func TestFormatJSON(t *testing.T) {
	out, err := FormatJSON([]Device{{Name: "dev1", IP: "10.0.0.1", Port: 3333, RAWPort: 3334, Baud: 115200}})
	if err != nil {
		t.Fatalf("FormatJSON: %v", err)
	}
	var devs []Device
	if err := json.Unmarshal([]byte(out), &devs); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(devs) != 1 || devs[0].Name != "dev1" || devs[0].Port != 3333 {
		t.Fatalf("roundtrip = %+v", devs)
	}

	empty, err := FormatJSON(nil)
	if err != nil {
		t.Fatalf("FormatJSON(nil): %v", err)
	}
	if strings.TrimSpace(empty) != "[]" {
		t.Fatalf("FormatJSON(nil) = %q, want []", empty)
	}
}

func TestSortAndDedupe(t *testing.T) {
	devs := sortDevices([]Device{
		{Name: "b", IP: "10.0.0.2", Port: 3333},
		{Name: "a", IP: "10.0.0.3", Port: 3333},
		{Name: "b", IP: "10.0.0.2", Port: 3333},
	})
	if len(devs) != 2 {
		t.Fatalf("dedupe failed: %+v", devs)
	}
	if devs[0].Name != "a" || devs[1].Name != "b" {
		t.Fatalf("sort failed: %+v", devs)
	}
}

// ---------------------------------------------------------------------------
// Discover collection logic (no multicast involved)
// ---------------------------------------------------------------------------

type fakeBrowser struct {
	entries []*zeroconf.ServiceEntry
	delay   time.Duration
	err     error
	// keepOpen leaves the entries channel open after delivering (models a
	// browser still listening until the context expires).
	keepOpen bool
	returns  chan struct{} // closed once Browse returns
}

func (f *fakeBrowser) Browse(ctx context.Context, service, domain string, entries chan<- *zeroconf.ServiceEntry) error {
	if f.returns == nil {
		f.returns = make(chan struct{})
	}
	defer close(f.returns)
	if f.err != nil {
		return f.err
	}
	// The real resolver returns after sending the query while entries are
	// delivered asynchronously; mimic that.
	go func() {
		if f.delay > 0 {
			select {
			case <-time.After(f.delay):
			case <-ctx.Done():
				return
			}
		}
		for _, e := range f.entries {
			select {
			case entries <- e:
			case <-ctx.Done():
				return
			}
		}
		if !f.keepOpen {
			close(entries)
		}
	}()
	return nil
}

func withFakeBrowser(t *testing.T, f *fakeBrowser) {
	t.Helper()
	old := newBrowser
	newBrowser = func() (mdnsBrowser, error) { return f, nil }
	t.Cleanup(func() { newBrowser = old })
}

// TestDiscoverWaitsForAsyncEntries guards against regressing to the bug where
// Discover returned as soon as Browse() returned (which happens immediately,
// before any mDNS response arrives).
func TestDiscoverWaitsForAsyncEntries(t *testing.T) {
	withFakeBrowser(t, &fakeBrowser{entries: []*zeroconf.ServiceEntry{testEntry()}, delay: 50 * time.Millisecond})
	devs, err := Discover(context.Background(), 2*time.Second, nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(devs) != 1 || devs[0].Name != "车间串口" {
		t.Fatalf("Discover = %+v, want one device", devs)
	}
}

func TestDiscoverBrowseError(t *testing.T) {
	withFakeBrowser(t, &fakeBrowser{err: errors.New("no multicast")})
	if _, err := Discover(context.Background(), time.Second, nil); err == nil {
		t.Fatal("Discover returned nil error for a failing browser")
	}
}

func TestDiscoverTimeoutIsNotError(t *testing.T) {
	withFakeBrowser(t, &fakeBrowser{keepOpen: true})
	start := time.Now()
	devs, err := Discover(context.Background(), 80*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(devs) != 0 {
		t.Fatalf("Discover = %+v, want none", devs)
	}
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
		t.Fatalf("Discover returned before the timeout: %s", elapsed)
	}
}

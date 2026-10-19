// Package discovery finds devices advertising the _wserial._tcp mDNS service.
package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/grandcat/zeroconf"

	wlog "github.com/wireless-serial/wserial/internal/log"
)

// ServiceType is the mDNS service advertised by the firmware.
const ServiceType = "_wserial._tcp"

// mdnsBrowser is the subset of *zeroconf.Resolver used by Discover, kept as an
// interface so the collection logic can be unit-tested without multicast.
type mdnsBrowser interface {
	Browse(ctx context.Context, service, domain string, entries chan<- *zeroconf.ServiceEntry) error
}

// newBrowser is a test hook; nil means the real zeroconf resolver.
var newBrowser = func() (mdnsBrowser, error) {
	return zeroconf.NewResolver(nil)
}

// Domain is the mDNS domain.
const Domain = "local."

// Device is one discovered wireless-serial device.
type Device struct {
	Name     string            `json:"name"`
	Hostname string            `json:"hostname,omitempty"`
	IP       string            `json:"ip"`
	Port     int               `json:"port"`               // WSP port (3333 by default)
	RAWPort  int               `json:"raw_port,omitempty"` // RAW port (3334 by default)
	Baud     uint32            `json:"baud,omitempty"`
	MAC      string            `json:"mac,omitempty"`
	Proto    int               `json:"proto,omitempty"`
	TXT      map[string]string `json:"txt,omitempty"`
}

// Address returns the host:port to dial for the WSP service.
func (d Device) Address() string {
	host := d.IP
	if host == "" {
		host = d.Hostname
	}
	return joinHostPort(host, d.Port)
}

// RawAddress returns the host:port to dial for the RAW service. It falls back
// to the conventional 3334 when the TXT record does not advertise one.
func (d Device) RawAddress() string {
	host := d.IP
	if host == "" {
		host = d.Hostname
	}
	port := d.RAWPort
	if port == 0 {
		port = 3334
	}
	return joinHostPort(host, port)
}

func joinHostPort(host string, port int) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + strconv.Itoa(port)
	}
	return host + ":" + strconv.Itoa(port)
}

// Discover browses for _wserial._tcp for at most timeout and returns what was
// found. A timeout with no result is not an error; transport failures are.
func Discover(ctx context.Context, timeout time.Duration, logger *wlog.Logger) ([]Device, error) {
	if logger == nil {
		logger = wlog.Discard()
	}
	resolver, err := newBrowser()
	if err != nil {
		return nil, fmt.Errorf("初始化 mDNS 解析器失败: %w", err)
	}
	logger.Debugf("mDNS: 正在浏览 %s.%s（%s）", ServiceType, Domain, timeout)

	browseCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	entries := make(chan *zeroconf.ServiceEntry, 32)
	browseResult := make(chan error, 1)
	go func() {
		browseResult <- resolver.Browse(browseCtx, ServiceType, Domain, entries)
	}()

	// resolver.Browse only sends the initial query and then returns; entries
	// keep arriving asynchronously until the context is cancelled (or the
	// browser fails later and closes the channel). So a nil Browse result must
	// not end the discovery window.
	var devs []Device
	for {
		select {
		case entry, ok := <-entries:
			if !ok {
				return sortDevices(devs), nil
			}
			if entry == nil {
				continue
			}
			d := parseEntry(entry)
			if d.Port <= 0 {
				continue
			}
			logger.Tracef("mDNS: 发现 %s %s:%d", d.Name, d.IP, d.Port)
			devs = append(devs, d)
		case err := <-browseResult:
			browseResult = nil // only the first result matters
			if err != nil {
				if browseCtx.Err() != nil {
					return sortDevices(devs), nil
				}
				return sortDevices(devs), fmt.Errorf("mDNS 浏览失败: %w", err)
			}
		case <-browseCtx.Done():
			return sortDevices(devs), nil
		}
	}
}

func parseEntry(entry *zeroconf.ServiceEntry) Device {
	txt := parseTXT(entry.Text)
	d := Device{
		Name:     entry.Instance,
		Hostname: strings.TrimSuffix(entry.HostName, "."),
		Port:     entry.Port,
		TXT:      txt,
	}
	if name := txt["name"]; name != "" {
		d.Name = name
	}
	if v, err := strconv.Atoi(txt["raw"]); err == nil {
		d.RAWPort = v
	}
	if v, err := strconv.ParseUint(txt["baud"], 10, 32); err == nil {
		d.Baud = uint32(v)
	}
	if v, err := strconv.Atoi(txt["proto"]); err == nil {
		d.Proto = v
	}
	d.MAC = strings.ToUpper(txt["mac"])
	if len(entry.AddrIPv4) > 0 {
		d.IP = entry.AddrIPv4[0].String()
	} else if len(entry.AddrIPv6) > 0 {
		d.IP = entry.AddrIPv6[0].String()
	}
	return d
}

func parseTXT(records []string) map[string]string {
	m := make(map[string]string, len(records))
	for _, r := range records {
		k, v, ok := strings.Cut(r, "=")
		if !ok {
			continue
		}
		m[strings.ToLower(strings.TrimSpace(k))] = v
	}
	return m
}

func sortDevices(devs []Device) []Device {
	sort.SliceStable(devs, func(i, j int) bool {
		if devs[i].Name != devs[j].Name {
			return devs[i].Name < devs[j].Name
		}
		return devs[i].IP < devs[j].IP
	})
	return dedupe(devs)
}

func dedupe(devs []Device) []Device {
	out := devs[:0]
	seen := make(map[string]bool, len(devs))
	for _, d := range devs {
		key := fmt.Sprintf("%s|%s|%d", d.Name, d.IP, d.Port)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, d)
	}
	return out
}

// FormatTable renders devices as an aligned table with human readable headers.
func FormatTable(devs []Device) string {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "名称\tIP\t端口\tMAC\t波特率")
	for _, d := range devs {
		ip := d.IP
		if ip == "" {
			ip = d.Hostname
		}
		mac := d.MAC
		if mac == "" {
			mac = "-"
		}
		baud := "-"
		if d.Baud != 0 {
			baud = strconv.FormatUint(uint64(d.Baud), 10)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", d.Name, ip, d.Port, mac, baud)
	}
	_ = tw.Flush()
	return b.String()
}

// FormatJSON renders devices as a JSON array (pretty printed).
func FormatJSON(devs []Device) (string, error) {
	if devs == nil {
		devs = []Device{}
	}
	b, err := json.MarshalIndent(devs, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b) + "\n", nil
}

package network

import (
	"bufio"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Service is one entry of `networksetup -listnetworkserviceorder`.
type Service struct {
	// Name is the service name, e.g. "iPhone USB". Always taken from the
	// "(N) Name" line, never from the "(Hardware Port: ...)" line below it
	// (for Tailscale they differ).
	Name string
	// HardwarePort and Device come from the "(Hardware Port: X, Device: Y)"
	// line. Device can be empty (e.g. VPN services).
	HardwarePort string
	Device       string
	// Enabled is false for services listed as "(*) Name".
	Enabled bool
}

var (
	serviceLineRe = regexp.MustCompile(`^\((\d+|\*)\)\s+(.+?)\s*$`)
	portLineRe    = regexp.MustCompile(`^\(Hardware Port: (.*), Device: ?(.*)\)\s*$`)
)

// ParseServiceOrder parses `networksetup -listnetworkserviceorder`:
//
//	An asterisk (*) denotes that a network service is disabled.
//	(1) Wi-Fi
//	(Hardware Port: Wi-Fi, Device: en0)
//
//	(*) iPhone USB
//	(Hardware Port: iPhone USB, Device: en10)
func ParseServiceOrder(out string) ([]Service, error) {
	if err := outputError(out); err != nil {
		return nil, err
	}
	var svcs []Service
	for _, line := range lines(out) {
		if m := portLineRe.FindStringSubmatch(line); m != nil {
			if len(svcs) > 0 {
				svcs[len(svcs)-1].HardwarePort = m[1]
				svcs[len(svcs)-1].Device = strings.TrimSpace(m[2])
			}
			continue
		}
		if m := serviceLineRe.FindStringSubmatch(line); m != nil {
			name, enabled := m[2], m[1] != "*"
			if strings.HasPrefix(name, "*") { // tolerate "(1) *Name" too
				name, enabled = strings.TrimSpace(name[1:]), false
			}
			svcs = append(svcs, Service{Name: name, Enabled: enabled})
		}
	}
	if len(svcs) == 0 {
		return nil, fmt.Errorf("no services in output %q", firstLine(out))
	}
	return svcs, nil
}

// HardwarePort is one entry of `networksetup -listallhardwareports`.
// Ports such as "iPhone USB" are listed only while the device is attached.
type HardwarePort struct {
	Port   string
	Device string
}

// ParseHardwarePorts parses `networksetup -listallhardwareports`.
func ParseHardwarePorts(out string) ([]HardwarePort, error) {
	if err := outputError(out); err != nil {
		return nil, err
	}
	var ports []HardwarePort
	for _, line := range lines(out) {
		if v, ok := strings.CutPrefix(line, "Hardware Port: "); ok {
			ports = append(ports, HardwarePort{Port: strings.TrimSpace(v)})
		} else if v, ok := strings.CutPrefix(line, "Device: "); ok && len(ports) > 0 {
			ports[len(ports)-1].Device = strings.TrimSpace(v)
		}
	}
	if len(ports) == 0 {
		return nil, fmt.Errorf("no hardware ports in output %q", firstLine(out))
	}
	return ports, nil
}

// ParseRoute parses `route -n get [-ifscope IF] default`. Without a
// default route it prints "not in table", sometimes with exit status 0,
// so absence is decided by the missing "interface:" line.
func ParseRoute(out string) (iface, gateway string) {
	for _, line := range lines(out) {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		switch k {
		case "interface":
			iface = strings.TrimSpace(v)
		case "gateway":
			gateway = strings.TrimSpace(v)
		}
	}
	if iface == "" {
		gateway = ""
	}
	return iface, gateway
}

// ParseIfconfig extracts the link status ("active"/"inactive", "" if not
// shown) and first IPv4 address from `ifconfig IF`.
func ParseIfconfig(out string) (status, ipv4 string) {
	for _, line := range lines(out) {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "status:":
			status = f[1]
		case "inet":
			if ipv4 == "" {
				ipv4 = f[1]
			}
		}
	}
	return status, ipv4
}

var airportRe = regexp.MustCompile(`Wi-Fi Power \([^)]*\): (On|Off)`)

// ParseAirportPower parses `networksetup -getairportpower IF`
// ("Wi-Fi Power (en0): On").
func ParseAirportPower(out string) (bool, error) {
	if err := outputError(out); err != nil {
		return false, err
	}
	m := airportRe.FindStringSubmatch(out)
	if m == nil {
		return false, fmt.Errorf("unexpected Wi-Fi power output %q", firstLine(out))
	}
	return m[1] == "On", nil
}

// ParseServiceEnabled parses `networksetup -getnetworkserviceenabled SVC`.
func ParseServiceEnabled(out string) (bool, error) {
	if err := outputError(out); err != nil {
		return false, err
	}
	switch strings.TrimSpace(out) {
	case "Enabled":
		return true, nil
	case "Disabled":
		return false, nil
	}
	return false, fmt.Errorf("unexpected service state output %q", firstLine(out))
}

// ParseIOUSBHasIPhone reports whether `ioreg -p IOUSB -w0` lists an iPhone
// (USB device nodes are named after the product, e.g. "iPhone@00100000").
func ParseIOUSBHasIPhone(out string) bool {
	for _, line := range lines(out) {
		_, node, ok := strings.Cut(line, "+-o ")
		if ok && strings.HasPrefix(strings.ToLower(node), "iphone") {
			return true
		}
	}
	return false
}

// errNetworksetup marks failures networksetup reported in its output.
var errNetworksetup = errors.New("networksetup error")

// outputError detects networksetup failure text. networksetup exits 0 on
// some failures (and non-zero on others), so its output is the source of
// truth, e.g.:
//
//	No Such Service is not a recognized network service.
//	** Error: The parameters were not valid.
func outputError(out string) error {
	var msgs []string
	for _, line := range lines(out) {
		l := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(l, "** Error"),
			strings.HasPrefix(l, "Error:"),
			strings.Contains(l, "is not a recognized network service"),
			strings.Contains(l, "is not a Wi-Fi interface"),
			strings.HasPrefix(l, "You cannot"),
			strings.Contains(l, "requires admin"),
			strings.Contains(l, "must be run as root"):
			msgs = append(msgs, l)
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", errNetworksetup, strings.Join(msgs, " "))
}

func lines(s string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

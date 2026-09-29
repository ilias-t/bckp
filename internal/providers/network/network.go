// Package network implements contracts.Network with route, ifconfig, ping,
// ioreg, networksetup and an HTTP reachability probe. It needs no root;
// the setters need the user to be in the admin group.
package network

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/paths"
)

// Defaults for empty Options fields.
const (
	// DefaultService is the iPhone USB tethering service name. It is the
	// service name exactly, not the hardware port line under it.
	DefaultService = "iPhone USB"
	// WiFiPort is the hardware port name of the Wi-Fi device.
	WiFiPort = "Wi-Fi"
	// iPhoneGateway is the gateway iOS Personal Hotspot always uses; it is
	// the last-resort guess when no route names the gateway.
	iPhoneGateway = contracts.HotspotGateway
	iPhoneSubnet  = "172.20.10."
)

// Options configures New. Empty fields are autodetected.
type Options struct {
	// Service is the hotspot network service; "" means DefaultService.
	Service string
	// Iface is the hotspot BSD interface; "" resolves it from Service via
	// -listnetworkserviceorder (which lists it even while the iPhone is
	// unplugged), then -listallhardwareports.
	Iface string
	// Gateway is the hotspot gateway; "" discovers it from the routing
	// table, falling back to 172.20.10.1 when the interface has a hotspot
	// address.
	Gateway string
	// WiFiIface is the Wi-Fi device; "" resolves it from the "Wi-Fi"
	// hardware port.
	WiFiIface string

	// NtfyServer is the first probe target (its /v1/health); "" means
	// DefaultNtfyServer. Ignored when Probe is set.
	NtfyServer string
	// Probe overrides the reachability probe (tests, or a custom policy);
	// nil means HTTPProbe(nil, DefaultTargets(NtfyServer)...).
	Probe Prober
}

// Verification of setters: networksetup can lag, so the read-back is
// retried.
const (
	verifyAttempts = 3
	verifyDelay    = 500 * time.Millisecond
)

// Net is the real contracts.Network. It holds no mutable state and is safe
// for concurrent use.
type Net struct {
	exec  contracts.Exec
	opts  Options
	probe Prober

	// Test seams.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

var _ contracts.Network = (*Net)(nil)

// New returns a Network. See Options for autodetection.
func New(exec contracts.Exec, opts Options) contracts.Network {
	if opts.Service == "" {
		opts.Service = DefaultService
	}
	probe := opts.Probe
	if probe == nil {
		probe = HTTPProbe(nil, DefaultTargets(opts.NtfyServer)...)
	}
	return &Net{exec: exec, opts: opts, probe: probe, now: time.Now, sleep: sleepCtx}
}

// Command argv (after the binary), shared with tests so fake keys match.
var (
	argsServiceOrder  = []string{"-listnetworkserviceorder"}
	argsHardwarePorts = []string{"-listallhardwareports"}
	argsRouteDefault  = []string{"-n", "get", "default"}
	argsIOUSB         = []string{"-p", "IOUSB", "-w0"}
)

func argsRouteScoped(iface string) []string {
	return []string{"-n", "get", "-ifscope", iface, "default"}
}
func argsPing(host string) []string { return []string{"-c1", "-t2", host} }

// argsDig resolves host with a 2s, single-try budget, through server
// ("@1.1.1.1") or, with server "", the system's primary resolver (the
// first nameserver in /etc/resolv.conf, which macOS keeps in step with
// `scutil --dns` resolver #1). dig never answers from a cache.
func argsDig(host, server string) []string {
	args := []string{"+short", "+time=2", "+tries=1", host}
	if server != "" {
		args = append(args, "@"+server)
	}
	return args
}

// Degraded checks (NetSnapshot.Checks): run only when not Online.
const (
	// PublicIP is pinged for ip_ok and queried for dns_direct.
	PublicIP = "1.1.1.1"
	// checkTimeout backstops each check; dig and ping bound themselves at
	// 2s. A check cut short by it (or by the snapshot's deadline) is
	// CheckSkip, not CheckFail.
	checkTimeout = 2500 * time.Millisecond
	// checkMargin is left for the rest of Snapshot; with less than
	// checkMin to spare the checks are skipped.
	checkMargin = 250 * time.Millisecond
	checkMin    = 500 * time.Millisecond
)

// ---- Snapshot ----

// Snapshot probes connectivity. Individual command failures are recorded
// in Diag; it returns an error only when ctx ends.
func (n *Net) Snapshot(ctx context.Context) (contracts.NetSnapshot, error) {
	d := &diag{}
	s := contracts.NetSnapshot{}

	svcs, svcErr := n.services(ctx)
	d.err("service order", svcErr)
	ports, portErr := n.hardwarePorts(ctx)
	d.err("hardware ports", portErr)

	svc, svcFound := findService(svcs, n.opts.Service)
	s.Iface = n.opts.Iface
	if s.Iface == "" {
		s.Iface = svc.Device
	}
	if s.Iface == "" {
		s.Iface = portDevice(ports, n.opts.Service)
	}
	hwPort := hasPort(ports, svc.HardwarePort, n.opts.Service, s.Iface)
	wifi := n.opts.WiFiIface
	if wifi == "" {
		wifi = wifiDevice(ports, svcs)
	}

	// Default route.
	var defaultGW string
	if out, err := n.run(ctx, paths.BinRoute, argsRouteDefault...); err != nil && !strings.Contains(out, "not in table") {
		d.err("route", err)
	} else {
		s.DefaultIface, defaultGW = ParseRoute(out)
	}
	s.DefaultGateway = defaultGW
	s.WiFiIface = wifi

	// Hotspot interface.
	status := ""
	if s.Iface != "" {
		out, err := n.run(ctx, paths.BinIfconfig, s.Iface)
		if err == nil {
			s.IfacePresent = true
			status, s.IfaceIPv4 = ParseIfconfig(out)
		} else if !strings.Contains(out, "does not exist") {
			d.err("ifconfig "+s.Iface, err)
		}
	}

	// Gateway.
	s.Gateway = n.opts.Gateway
	if s.Gateway == "" && s.Iface != "" && s.DefaultIface == s.Iface {
		s.Gateway = defaultGW
	}
	if s.Gateway == "" && s.IfacePresent {
		out, _ := n.run(ctx, paths.BinRoute, argsRouteScoped(s.Iface)...)
		if ifc, gw := ParseRoute(out); ifc == s.Iface {
			s.Gateway = gw
		}
	}
	if s.Gateway == "" && strings.HasPrefix(s.IfaceIPv4, iPhoneSubnet) {
		s.Gateway = iPhoneGateway
	}
	pingGW := "skip"
	if s.Gateway != "" && s.IfacePresent {
		_, err := n.run(ctx, paths.BinPing, argsPing(s.Gateway)...)
		s.GatewayReachable = err == nil
		pingGW = map[bool]string{true: "ok", false: "fail"}[s.GatewayReachable]
	}

	// iPhone on USB.
	usb := false
	if out, err := n.run(ctx, paths.BinIoreg, argsIOUSB...); err != nil {
		d.err("ioreg IOUSB", err)
	} else {
		usb = ParseIOUSBHasIPhone(out)
	}
	s.IPhoneUSBPresent = svcFound && (hwPort || usb)

	// Wi-Fi.
	wifiDesc := "-"
	if wifi != "" {
		on, err := n.wifiPower(ctx, wifi)
		d.err("wifi power", err)
		s.WiFiOn = on
		wifiDesc = map[bool]string{true: "On", false: "Off"}[on]
		if err != nil {
			wifiDesc = "?"
		}
		if out, err := n.run(ctx, paths.BinIfconfig, wifi); err == nil {
			if st, _ := ParseIfconfig(out); st != "" {
				wifiDesc += "/" + st
			}
		}
	}

	// End-to-end reachability: only worth probing with a default route.
	probeDetail := "skipped: no default route"
	if s.DefaultIface != "" {
		s.Online, probeDetail = n.probe(ctx)
	}

	lines := []string{"probe: " + probeDetail}
	if !s.Online {
		s.Checks = n.degradedChecks(ctx)
		lines = append(lines, checksLine(*s.Checks))
	}

	if err := ctx.Err(); err != nil {
		return contracts.NetSnapshot{}, fmt.Errorf("network: snapshot: %w", err)
	}
	s.At = n.now()
	s.Diag = d.render(append([]string{fmt.Sprintf(
		"route=%s %s=%s status=%s ipv4=%s gw=%s ping_gw=%s iphone_usb=%s hwport=%s service=%q wifi=%s(%s) online=%s via=%s default_gw=%s",
		or(s.DefaultIface, "none"), or(s.Iface, "?"), yesMissing(s.IfacePresent), or(status, "-"),
		or(s.IfaceIPv4, "-"), or(s.Gateway, "?"), pingGW, yesNo(usb), yesNo(hwPort),
		serviceDesc(n.opts.Service, svcFound, svc), wifiDesc, or(wifi, "?"), yesNo(s.Online),
		s.Link(), or(s.DefaultGateway, "-"),
	)}, lines...)...)
	return s, nil
}

// degradedChecks runs the DNS/IP checks concurrently (about 2s in all):
// the probe host through the system resolver and through PublicIP, and a
// ping to PublicIP. They share what is left of ctx's deadline.
func (n *Net) degradedChecks(ctx context.Context) *contracts.NetChecks {
	c := &contracts.NetChecks{DNSSys: contracts.CheckSkip, DNSDirect: contracts.CheckSkip, IPOK: contracts.CheckSkip}
	budget := checkTimeout
	if dl, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(dl)-checkMargin)
	}
	if budget < checkMin {
		return c
	}
	cctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	host := probeHost(n.opts.NtfyServer)
	var wg sync.WaitGroup
	check := func(dst *string, ok func(string, error) bool, name string, args ...string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := n.run(cctx, name, args...)
			switch {
			case ok(out, err):
				*dst = contracts.CheckOK
			case cctx.Err() == nil:
				*dst = contracts.CheckFail
			}
		}()
	}
	check(&c.DNSSys, digOK, paths.BinDig, argsDig(host, "")...)
	check(&c.DNSDirect, digOK, paths.BinDig, argsDig(host, PublicIP)...)
	check(&c.IPOK, func(_ string, err error) bool { return err == nil }, paths.BinPing, argsPing(PublicIP)...)
	wg.Wait()
	return c
}

// digOK: dig exited 0 and printed an address (+short prints nothing for
// NXDOMAIN, and CNAME targets before the address).
func digOK(out string, err error) bool {
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if net.ParseIP(strings.TrimSpace(line)) != nil {
			return true
		}
	}
	return false
}

// probeHost is the host the reachability probe tries first (the ntfy
// server's).
func probeHost(ntfyServer string) string {
	if ntfyServer == "" {
		ntfyServer = DefaultNtfyServer
	}
	if u, err := url.Parse(ntfyServer); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return "ntfy.sh"
}

func checksLine(c contracts.NetChecks) string {
	line := fmt.Sprintf("checks: dns_sys=%s dns_direct=%s ip_ok=%s", c.DNSSys, c.DNSDirect, c.IPOK)
	if v := c.Verdict(); v != "" {
		line += " (" + v + ")"
	}
	return line
}

func serviceDesc(name string, found bool, svc Service) string {
	switch {
	case !found:
		return name + " (missing)"
	case !svc.Enabled:
		return name + " (disabled)"
	}
	return name
}

// ---- service order ----

// ServiceOrder returns service names in priority order, without the "*"
// disabled marker.
func (n *Net) ServiceOrder(ctx context.Context) ([]string, error) {
	svcs, err := n.services(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(svcs))
	for i, s := range svcs {
		names[i] = s.Name
	}
	return names, nil
}

// SetServiceOrder applies order (a permutation of ServiceOrder) and
// verifies it by re-reading.
func (n *Net) SetServiceOrder(ctx context.Context, order []string) error {
	cur, err := n.ServiceOrder(ctx)
	if err != nil {
		return err
	}
	a, b := slices.Clone(order), slices.Clone(cur)
	slices.Sort(a)
	slices.Sort(b)
	if !slices.Equal(a, b) {
		return fmt.Errorf("network: service order %q is not a permutation of %q", order, cur)
	}
	if slices.Equal(order, cur) {
		return nil
	}
	if _, err := n.ns(ctx, append([]string{"-ordernetworkservices"}, order...)...); err != nil {
		return err
	}
	return n.verify(ctx, "service order", func() (bool, string, error) {
		got, err := n.ServiceOrder(ctx)
		return slices.Equal(got, order), fmt.Sprintf("%q", got), err
	})
}

// SetServiceEnabled enables or disables service and verifies the result.
func (n *Net) SetServiceEnabled(ctx context.Context, service string, on bool) error {
	if _, err := n.ns(ctx, "-setnetworkserviceenabled", service, onOff(on)); err != nil {
		return err
	}
	return n.verify(ctx, fmt.Sprintf("service %q enabled=%t", service, on), func() (bool, string, error) {
		out, err := n.ns(ctx, "-getnetworkserviceenabled", service)
		if err != nil {
			return false, "", err
		}
		got, err := ParseServiceEnabled(out)
		return got == on, fmt.Sprintf("enabled=%t", got), err
	})
}

// ---- Wi-Fi ----

// WiFiPower reports Wi-Fi radio power.
func (n *Net) WiFiPower(ctx context.Context) (bool, error) {
	wifi, err := n.wifiIface(ctx)
	if err != nil {
		return false, err
	}
	return n.wifiPower(ctx, wifi)
}

// SetWiFiPower turns the Wi-Fi radio on or off and verifies the result.
func (n *Net) SetWiFiPower(ctx context.Context, on bool) error {
	wifi, err := n.wifiIface(ctx)
	if err != nil {
		return err
	}
	if _, err := n.ns(ctx, "-setairportpower", wifi, onOff(on)); err != nil {
		return err
	}
	return n.verify(ctx, fmt.Sprintf("wifi %s power=%s", wifi, onOff(on)), func() (bool, string, error) {
		got, err := n.wifiPower(ctx, wifi)
		return got == on, "power=" + onOff(got), err
	})
}

func (n *Net) wifiPower(ctx context.Context, wifi string) (bool, error) {
	out, err := n.ns(ctx, "-getairportpower", wifi)
	if err != nil {
		return false, err
	}
	on, err := ParseAirportPower(out)
	if err != nil {
		return false, fmt.Errorf("network: %w", err)
	}
	return on, nil
}

func (n *Net) wifiIface(ctx context.Context) (string, error) {
	if n.opts.WiFiIface != "" {
		return n.opts.WiFiIface, nil
	}
	ports, perr := n.hardwarePorts(ctx)
	if dev := wifiDevice(ports, nil); dev != "" {
		return dev, nil
	}
	svcs, serr := n.services(ctx)
	if dev := wifiDevice(nil, svcs); dev != "" {
		return dev, nil
	}
	return "", fmt.Errorf("network: no %q hardware port found (ports: %v; services: %v)", WiFiPort, perr, serr)
}

// ---- helpers ----

func (n *Net) services(ctx context.Context) ([]Service, error) {
	out, err := n.ns(ctx, argsServiceOrder...)
	if err != nil {
		return nil, err
	}
	svcs, err := ParseServiceOrder(out)
	if err != nil {
		return nil, fmt.Errorf("network: %w", err)
	}
	return svcs, nil
}

func (n *Net) hardwarePorts(ctx context.Context) ([]HardwarePort, error) {
	out, err := n.ns(ctx, argsHardwarePorts...)
	if err != nil {
		return nil, err
	}
	ports, err := ParseHardwarePorts(out)
	if err != nil {
		return nil, fmt.Errorf("network: %w", err)
	}
	return ports, nil
}

// run executes a command and returns stdout+stderr with the exec error.
func (n *Net) run(ctx context.Context, name string, args ...string) (string, error) {
	res, err := n.exec.Run(ctx, name, args...)
	return string(res.Stdout) + string(res.Stderr), err
}

// ns runs networksetup. Its output is checked for error text regardless
// of the exit status, because networksetup exits 0 on some failures.
func (n *Net) ns(ctx context.Context, args ...string) (string, error) {
	out, err := n.run(ctx, paths.BinNetworksetup, args...)
	if perr := outputError(out); perr != nil {
		return out, fmt.Errorf("network: networksetup %s: %w", args[0], perr)
	}
	if err != nil {
		return out, fmt.Errorf("network: %w", err)
	}
	return out, nil
}

// verify re-reads state until check reports success, up to verifyAttempts.
func (n *Net) verify(ctx context.Context, what string, check func() (ok bool, got string, err error)) error {
	var got string
	var err error
	for i := range verifyAttempts {
		if i > 0 {
			if serr := n.sleep(ctx, verifyDelay); serr != nil {
				return fmt.Errorf("network: verify %s: %w", what, serr)
			}
		}
		var ok bool
		ok, got, err = check()
		if err == nil && ok {
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("network: verify %s: %w", what, err)
	}
	return fmt.Errorf("network: %s not applied (read back %s)", what, got)
}

func findService(svcs []Service, name string) (Service, bool) {
	for _, s := range svcs {
		if s.Name == name {
			return s, true
		}
	}
	return Service{}, false
}

func portDevice(ports []HardwarePort, port string) string {
	for _, p := range ports {
		if p.Port == port {
			return p.Device
		}
	}
	return ""
}

// hasPort reports whether the service's hardware is attached: its port (or
// device) appears in -listallhardwareports.
func hasPort(ports []HardwarePort, portName, service, iface string) bool {
	for _, p := range ports {
		if (portName != "" && p.Port == portName) || p.Port == service || (iface != "" && p.Device == iface) {
			return true
		}
	}
	return false
}

func wifiDevice(ports []HardwarePort, svcs []Service) string {
	if dev := portDevice(ports, WiFiPort); dev != "" {
		return dev
	}
	for _, s := range svcs {
		if s.HardwarePort == WiFiPort && s.Device != "" {
			return s.Device
		}
	}
	return ""
}

// diag collects the one-line summary, extra lines, and command errors.
type diag struct{ errs []string }

func (d *diag) err(what string, err error) {
	if err != nil {
		d.errs = append(d.errs, fmt.Sprintf("error: %s: %v", what, err))
	}
}

func (d *diag) render(lines ...string) string {
	return strings.Join(append(lines, d.errs...), "\n")
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func yesMissing(b bool) string {
	if b {
		return "yes"
	}
	return "missing"
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

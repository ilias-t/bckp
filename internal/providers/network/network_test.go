package network

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/fakes"
	"github.com/ilias-t/bckp/internal/paths"
)

// Fixtures were recorded on an Apple Silicon Mac (macOS 27.0) with
// read-only commands, with MAC and IP addresses, a USB serial and service
// names anonymized. At recording time the iPhone was unplugged and Wi-Fi
// (en0) was the default route. synth_* files are hand-made from those recordings for states that
// need the iPhone attached or a mutating command (never run here):
//   - synth_ifconfig_en10.txt, synth_route_default_en10.txt,
//     synth_ping_gw_ok.txt, synth_ioreg_iousb_iphone.txt,
//     synth_listallhardwareports_iphone.txt: iPhone tethered.
//   - synth_listnetworkserviceorder_iphone_disabled.txt: "(*)" marker.
//   - synth_setter_error.txt: networksetup error text on exit status 0.
//   - synth_getnetworkserviceenabled_disabled.txt,
//     synth_getairportpower_en0_off.txt: read-backs after a setter.
//   - synth_route_default_en0_hotspot.txt: the hotspot joined over Wi-Fi
//     (default route en0 via 172.20.10.1).
//   - synth_dig_timeout.txt (dig exit 9), synth_ping_1111_fail.txt (exit 2):
//     the DNS/IP checks failing. dig_short_ntfy_ok.txt,
//     dig_short_nxdomain.txt (empty, exit 0) and ping_1111_ok.txt are
//     recorded.

func fx(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func cl(name string, args ...string) string { return contracts.CommandLine(name, args...) }

var (
	cmdOrder     = cl(paths.BinNetworksetup, argsServiceOrder...)
	cmdPorts     = cl(paths.BinNetworksetup, argsHardwarePorts...)
	cmdRoute     = cl(paths.BinRoute, argsRouteDefault...)
	cmdRouteEn10 = cl(paths.BinRoute, argsRouteScoped("en10")...)
	cmdIfEn10    = cl(paths.BinIfconfig, "en10")
	cmdIfEn0     = cl(paths.BinIfconfig, "en0")
	cmdPingGW    = cl(paths.BinPing, argsPing("172.20.10.1")...)
	cmdIOUSB     = cl(paths.BinIoreg, argsIOUSB...)
	cmdAirport   = cl(paths.BinNetworksetup, "-getairportpower", "en0")
	cmdDigSys    = cl(paths.BinDig, argsDig("ntfy.sh", "")...)
	cmdDigDirect = cl(paths.BinDig, argsDig("ntfy.sh", PublicIP)...)
	cmdPingIP    = cl(paths.BinPing, argsPing(PublicIP)...)
	at           = time.Date(2026, 9, 28, 20, 0, 0, 0, time.UTC)
)

func TestCommandLines(t *testing.T) {
	want := []string{
		"/usr/sbin/networksetup -listnetworkserviceorder",
		"/usr/sbin/networksetup -listallhardwareports",
		"/sbin/route -n get default",
		"/sbin/route -n get -ifscope en10 default",
		"/sbin/ping -c1 -t2 172.20.10.1",
		"/usr/sbin/ioreg -p IOUSB -w0",
		"/usr/bin/dig +short +time=2 +tries=1 ntfy.sh",
		"/usr/bin/dig +short +time=2 +tries=1 ntfy.sh @1.1.1.1",
		"/sbin/ping -c1 -t2 1.1.1.1",
	}
	got := []string{cmdOrder, cmdPorts, cmdRoute, cmdRouteEn10, cmdPingGW, cmdIOUSB, cmdDigSys, cmdDigDirect, cmdPingIP}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
}

// stubProbe records calls and returns a fixed answer.
type stubProbe struct {
	ok    bool
	calls int
}

func (p *stubProbe) fn(ctx context.Context) (bool, string) {
	p.calls++
	if p.ok {
		return true, "ntfy.sh ok"
	}
	return false, "ntfy.sh: timeout; captive.apple.com: timeout"
}

func newNet(ex contracts.Exec, opts Options, probe *stubProbe) *Net {
	opts.Probe = probe.fn
	n := New(ex, opts).(*Net)
	n.now = func() time.Time { return at }
	n.sleep = func(context.Context, time.Duration) error { return nil }
	return n
}

// recordedExec is the machine as recorded: iPhone unplugged, Wi-Fi up.
func recordedExec(t *testing.T) *fakes.Exec {
	return fakes.NewExec().
		On(cmdOrder, fx(t, "listnetworkserviceorder.txt")).
		On(cmdPorts, fx(t, "listallhardwareports_no_iphone.txt")).
		On(cmdRoute, fx(t, "route_default_en0.txt")).
		OnFail(cmdIfEn10, 1, fx(t, "ifconfig_en10_missing.txt")).
		On(cmdIfEn0, fx(t, "ifconfig_en0.txt")).
		On(cmdIOUSB, fx(t, "ioreg_iousb_no_iphone.txt")).
		On(cmdAirport, fx(t, "getairportpower_en0_on.txt"))
}

func TestSnapshotRecordedNoIPhone(t *testing.T) {
	ex := recordedExec(t)
	p := &stubProbe{ok: true}
	s, err := newNet(ex, Options{}, p).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := contracts.NetSnapshot{
		DefaultIface: "en0", Online: true, Iface: "en10", WiFiOn: true, At: at,
		WiFiIface: "en0", DefaultGateway: "192.0.2.1",
	}
	diag := s.Diag
	s.Diag = ""
	if s != want {
		t.Errorf("got  %+v\nwant %+v", s, want)
	}
	for _, sub := range []string{"route=en0", "en10=missing", "ping_gw=skip", "iphone_usb=no", `service="iPhone USB"`, "wifi=On/active(en0)", "via=wifi default_gw=192.0.2.1", "probe: ntfy.sh ok"} {
		if !strings.Contains(diag, sub) {
			t.Errorf("diag missing %q:\n%s", sub, diag)
		}
	}
	if strings.Contains(diag, "error:") {
		t.Errorf("unexpected errors in diag:\n%s", diag)
	}
	if ex.CallCount(cmdPingGW) != 0 {
		t.Error("pinged a gateway with the hotspot interface absent")
	}
	if p.calls != 1 {
		t.Errorf("probe calls = %d", p.calls)
	}
}

// iphoneExec: iPhone tethered and the default route (synthetic).
func iphoneExec(t *testing.T) *fakes.Exec {
	return fakes.NewExec().
		On(cmdOrder, fx(t, "listnetworkserviceorder.txt")).
		On(cmdPorts, fx(t, "synth_listallhardwareports_iphone.txt")).
		On(cmdRoute, fx(t, "synth_route_default_en10.txt")).
		On(cmdIfEn10, fx(t, "synth_ifconfig_en10.txt")).
		On(cmdIfEn0, fx(t, "ifconfig_en0.txt")).
		On(cmdPingGW, fx(t, "synth_ping_gw_ok.txt")).
		On(cmdIOUSB, fx(t, "synth_ioreg_iousb_iphone.txt")).
		On(cmdAirport, fx(t, "getairportpower_en0_on.txt"))
}

func TestSnapshotIPhoneUp(t *testing.T) {
	s, err := newNet(iphoneExec(t), Options{}, &stubProbe{ok: true}).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := contracts.NetSnapshot{
		DefaultIface: "en10", Gateway: "172.20.10.1", Online: true, Iface: "en10",
		IfacePresent: true, IfaceIPv4: "172.20.10.2", GatewayReachable: true,
		IPhoneUSBPresent: true, WiFiOn: true, At: at,
		WiFiIface: "en0", DefaultGateway: "172.20.10.1",
	}
	diag := s.Diag
	s.Diag = ""
	if s != want {
		t.Errorf("got  %+v\nwant %+v", s, want)
	}
	if !strings.Contains(diag, "route=en10 en10=yes status=active ipv4=172.20.10.2 gw=172.20.10.1 ping_gw=ok iphone_usb=yes hwport=yes") {
		t.Errorf("diag:\n%s", diag)
	}
	if s.Link() != contracts.LinkHotspotUSB || !strings.Contains(diag, "via=hotspot_usb") {
		t.Errorf("link = %s, diag:\n%s", s.Link(), diag)
	}
}

// TestSnapshotHotspotOverWiFi: the phone is not plugged in; macOS joined
// its hotspot over Wi-Fi, so the default route is en0 via 172.20.10.1.
func TestSnapshotHotspotOverWiFi(t *testing.T) {
	ex := fakes.NewExec().
		On(cmdOrder, fx(t, "listnetworkserviceorder.txt")).
		On(cmdPorts, fx(t, "listallhardwareports_no_iphone.txt")).
		On(cmdRoute, fx(t, "synth_route_default_en0_hotspot.txt")).
		OnFail(cmdIfEn10, 1, fx(t, "ifconfig_en10_missing.txt")).
		On(cmdIfEn0, fx(t, "ifconfig_en0.txt")).
		On(cmdIOUSB, fx(t, "ioreg_iousb_no_iphone.txt")).
		On(cmdAirport, fx(t, "getairportpower_en0_on.txt"))
	s, err := newNet(ex, Options{}, &stubProbe{ok: true}).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.DefaultIface != "en0" || s.DefaultGateway != "172.20.10.1" || s.WiFiIface != "en0" || s.IPhoneUSBPresent {
		t.Fatalf("snapshot = %+v", s)
	}
	if s.Link() != contracts.LinkHotspotWiFi || !strings.Contains(s.Diag, "via=hotspot_wifi default_gw=172.20.10.1") {
		t.Errorf("link = %s, diag:\n%s", s.Link(), s.Diag)
	}
	if s.Gateway != "" || s.GatewayReachable {
		t.Errorf("the hotspot (USB) gateway fields must stay unset: %+v", s)
	}
}

// withChecks registers the degraded DNS/IP checks: system DNS, direct DNS
// and ping 1.1.1.1 each succeeding or failing.
func withChecks(t *testing.T, e *fakes.Exec, sys, direct, ip bool) *fakes.Exec {
	dig := func(cmd string, ok bool) {
		if ok {
			e.On(cmd, fx(t, "dig_short_ntfy_ok.txt"))
		} else {
			e.OnFail(cmd, 9, fx(t, "synth_dig_timeout.txt"))
		}
	}
	dig(cmdDigSys, sys)
	dig(cmdDigDirect, direct)
	if ip {
		e.On(cmdPingIP, fx(t, "ping_1111_ok.txt"))
	} else {
		e.OnFail(cmdPingIP, 2, fx(t, "synth_ping_1111_fail.txt"))
	}
	return e
}

func TestSnapshotRouteMissing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		route func(*fakes.Exec) *fakes.Exec
	}{
		// Recorded: "not in table" with exit status 0.
		{"exit 0", func(e *fakes.Exec) *fakes.Exec { return e.On(cmdRoute, fx(t, "route_not_in_table.txt")) }},
		{"exit 1", func(e *fakes.Exec) *fakes.Exec { return e.OnFail(cmdRoute, 1, fx(t, "route_not_in_table.txt")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// iPhone attached with an address but no route anywhere: the
			// gateway falls back to the hotspot convention and the ping
			// fails (recorded 100% loss, exit 2).
			ex := tc.route(fakes.NewExec()).
				On(cmdOrder, fx(t, "listnetworkserviceorder.txt")).
				On(cmdPorts, fx(t, "synth_listallhardwareports_iphone.txt")).
				On(cmdIfEn10, fx(t, "synth_ifconfig_en10.txt")).
				On(cmdRouteEn10, fx(t, "route_not_in_table.txt")).
				OnFail(cmdPingGW, 2, "").
				On(cmdIfEn0, fx(t, "ifconfig_en0.txt")).
				On(cmdIOUSB, fx(t, "synth_ioreg_iousb_iphone.txt")).
				On(cmdAirport, fx(t, "getairportpower_en0_on.txt"))
			withChecks(t, ex, false, false, false)
			p := &stubProbe{ok: true}
			s, err := newNet(ex, Options{}, p).Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if s.DefaultIface != "" || s.Online || s.GatewayReachable || !s.IfacePresent || !s.IPhoneUSBPresent {
				t.Errorf("snapshot = %+v", s)
			}
			if s.Gateway != "172.20.10.1" {
				t.Errorf("gateway = %q", s.Gateway)
			}
			if p.calls != 0 {
				t.Error("probed without a default route")
			}
			if !strings.Contains(s.Diag, "route=none") || !strings.Contains(s.Diag, "ping_gw=fail") ||
				!strings.Contains(s.Diag, "probe: skipped: no default route") || strings.Contains(s.Diag, "error:") ||
				!strings.Contains(s.Diag, "checks: dns_sys=fail dns_direct=fail ip_ok=fail (no IP connectivity)") {
				t.Errorf("diag:\n%s", s.Diag)
			}
		})
	}
}

func TestSnapshotOfflineProbeFails(t *testing.T) {
	ex := withChecks(t, recordedExec(t), true, true, true)
	s, err := newNet(ex, Options{}, &stubProbe{ok: false}).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Online || s.DefaultIface != "en0" || !strings.Contains(s.Diag, "probe: ntfy.sh: timeout") {
		t.Errorf("snapshot = %+v", s)
	}
}

// TestSnapshotDegradedChecks: when the HTTP probe fails, the DNS/IP checks
// tell a dead link from a broken system resolver (e.g. Tailscale's).
func TestSnapshotDegradedChecks(t *testing.T) {
	for _, tc := range []struct {
		name            string
		sys, direct, ip bool
		want            contracts.NetChecks
		line            string
	}{
		{"system DNS broken", false, true, true,
			contracts.NetChecks{DNSSys: "fail", DNSDirect: "ok", IPOK: "ok"},
			"checks: dns_sys=fail dns_direct=ok ip_ok=ok (system DNS failing, direct DNS works (VPN/Tailscale DNS?))"},
		{"all DNS broken", false, false, true,
			contracts.NetChecks{DNSSys: "fail", DNSDirect: "fail", IPOK: "ok"},
			"checks: dns_sys=fail dns_direct=fail ip_ok=ok (IP works, DNS failing)"},
		{"no IP", false, false, false,
			contracts.NetChecks{DNSSys: "fail", DNSDirect: "fail", IPOK: "fail"},
			"checks: dns_sys=fail dns_direct=fail ip_ok=fail (no IP connectivity)"},
		{"everything answers", true, true, true,
			contracts.NetChecks{DNSSys: "ok", DNSDirect: "ok", IPOK: "ok"},
			"checks: dns_sys=ok dns_direct=ok ip_ok=ok (IP and DNS work (HTTP probe blocked or captive portal?))"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex := withChecks(t, recordedExec(t), tc.sys, tc.direct, tc.ip)
			s, err := newNet(ex, Options{}, &stubProbe{ok: false}).Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if s.Checks == nil || *s.Checks != tc.want {
				t.Fatalf("checks = %+v, want %+v", s.Checks, tc.want)
			}
			if !strings.Contains(s.Diag, tc.line) {
				t.Errorf("diag lacks %q:\n%s", tc.line, s.Diag)
			}
		})
	}
}

// TestSnapshotChecksNXDOMAIN: dig exits 0 with no address for a name that
// does not resolve; that is a failure.
func TestSnapshotChecksNXDOMAIN(t *testing.T) {
	ex := recordedExec(t).On(cmdDigSys, fx(t, "dig_short_nxdomain.txt")).
		On(cmdDigDirect, fx(t, "dig_short_ntfy_ok.txt")).On(cmdPingIP, fx(t, "ping_1111_ok.txt"))
	s, err := newNet(ex, Options{}, &stubProbe{ok: false}).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Checks == nil || s.Checks.DNSSys != contracts.CheckFail || s.Checks.DNSDirect != contracts.CheckOK {
		t.Fatalf("checks = %+v", s.Checks)
	}
}

// TestSnapshotOnlineRunsNoChecks: the DNS/IP checks run only when degraded.
func TestSnapshotOnlineRunsNoChecks(t *testing.T) {
	ex := withChecks(t, recordedExec(t), true, true, true)
	s, err := newNet(ex, Options{}, &stubProbe{ok: true}).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Checks != nil || strings.Contains(s.Diag, "checks:") {
		t.Fatalf("checks while online: %+v\n%s", s.Checks, s.Diag)
	}
	for _, c := range []string{cmdDigSys, cmdDigDirect, cmdPingIP} {
		if n := ex.CallCount(c); n != 0 {
			t.Errorf("%s ran %d times while online", c, n)
		}
	}
}

// TestSnapshotChecksSkippedWithoutTime: with too little of the snapshot's
// deadline left the checks are skipped, not failed, and the snapshot
// still succeeds.
func TestSnapshotChecksSkippedWithoutTime(t *testing.T) {
	ex := withChecks(t, recordedExec(t), true, true, true)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	s, err := newNet(ex, Options{}, &stubProbe{ok: false}).Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := contracts.NetChecks{DNSSys: "skip", DNSDirect: "skip", IPOK: "skip"}
	if s.Checks == nil || *s.Checks != want || ex.CallCount(cmdPingIP) != 0 {
		t.Fatalf("checks = %+v", s.Checks)
	}
}

func TestProbeHost(t *testing.T) {
	for in, want := range map[string]string{"": "ntfy.sh", "https://ntfy.example.com:8443/": "ntfy.example.com", "::bad": "ntfy.sh"} {
		if got := probeHost(in); got != want {
			t.Errorf("probeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSnapshotCommandFailuresGoToDiag(t *testing.T) {
	// Every command fails; Snapshot still succeeds and explains why.
	ex := fakes.NewExec().
		OnFail(cmdOrder, 1, "boom").
		OnFail(cmdPorts, 1, "boom").
		OnFail(cmdRoute, 1, "route: something odd").
		OnFail(cmdIOUSB, 1, "ioreg: boom")
	s, err := newNet(ex, Options{Iface: "en10", WiFiIface: "en10"}, &stubProbe{ok: true}).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// ifconfig en10 and -getairportpower en10 are unregistered: fakes.Exec
	// errors, and the airport output parser sees no error text either.
	for _, sub := range []string{"error: service order", "error: hardware ports", "error: route", "error: ifconfig en10", "error: ioreg IOUSB", "error: wifi power", `service="iPhone USB (missing)"`} {
		if !strings.Contains(s.Diag, sub) {
			t.Errorf("diag missing %q:\n%s", sub, s.Diag)
		}
	}
	if s.Online || s.IPhoneUSBPresent {
		t.Errorf("snapshot = %+v", s)
	}
}

func TestSnapshotContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newNet(recordedExec(t), Options{}, &stubProbe{ok: true}).Snapshot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestSnapshotExplicitOptions(t *testing.T) {
	// Configured iface/gateway/wifi skip autodetection of those values.
	ex := recordedExec(t)
	s, err := newNet(ex, Options{Service: "iPhone USB", Iface: "en10", Gateway: "172.20.10.1", WiFiIface: "en0"}, &stubProbe{ok: true}).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Gateway != "172.20.10.1" || s.Iface != "en10" || s.GatewayReachable {
		t.Errorf("snapshot = %+v", s)
	}
}

func TestParseServiceOrder(t *testing.T) {
	svcs, err := ParseServiceOrder(fx(t, "listnetworkserviceorder.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Service{
		{"Display Controls", "Display Controls", "usbmodemXXXX0001", true},
		{"USB 10/100/1000 LAN", "USB 10/100/1000 LAN", "en7", true},
		{"Thunderbolt Ethernet", "Thunderbolt Ethernet", "en9", true},
		{"Wi-Fi", "Wi-Fi", "en0", true},
		{"Thunderbolt Bridge", "Thunderbolt Bridge", "bridge0", true},
		{"iPhone USB", "iPhone USB", "en10", true},
		{"Tailscale", "io.tailscale.ipn.macos", "", true},
	}
	if !slices.Equal(svcs, want) {
		t.Errorf("got  %+v\nwant %+v", svcs, want)
	}
	svcs, err = ParseServiceOrder(fx(t, "synth_listnetworkserviceorder_iphone_disabled.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := findService(svcs, "iPhone USB"); s.Enabled || s.Device != "en10" {
		t.Errorf("disabled iPhone USB = %+v", s)
	}
	svcs, _ = ParseServiceOrder("(1) *Old Style\n(Hardware Port: X, Device: en1)\n")
	if len(svcs) != 1 || svcs[0].Name != "Old Style" || svcs[0].Enabled {
		t.Errorf("old style = %+v", svcs)
	}
	for _, bad := range []string{"", "garbage\n", fx(t, "getnetworkserviceenabled_bogus_error.txt")} {
		if _, err := ParseServiceOrder(bad); err == nil {
			t.Errorf("ParseServiceOrder(%q) should fail", bad)
		}
	}
}

func TestServiceOrder(t *testing.T) {
	n := newNet(recordedExec(t), Options{}, &stubProbe{})
	got, err := n.ServiceOrder(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Display Controls", "USB 10/100/1000 LAN", "Thunderbolt Ethernet", "Wi-Fi", "Thunderbolt Bridge", "iPhone USB", "Tailscale"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q", got)
	}
	// Disabled services are listed without the marker.
	ex := fakes.NewExec().On(cmdOrder, fx(t, "synth_listnetworkserviceorder_iphone_disabled.txt"))
	got, _ = newNet(ex, Options{}, &stubProbe{}).ServiceOrder(context.Background())
	if !slices.Equal(got, want) {
		t.Errorf("disabled: got %q", got)
	}
}

func TestParseHardwarePorts(t *testing.T) {
	ports, err := ParseHardwarePorts(fx(t, "listallhardwareports_no_iphone.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != 8 || portDevice(ports, "Wi-Fi") != "en0" || portDevice(ports, "iPhone USB") != "" {
		t.Errorf("ports = %+v", ports)
	}
	ports, _ = ParseHardwarePorts(fx(t, "synth_listallhardwareports_iphone.txt"))
	if portDevice(ports, "iPhone USB") != "en10" {
		t.Errorf("ports = %+v", ports)
	}
}

func TestParseMisc(t *testing.T) {
	if i, g := ParseRoute(fx(t, "route_default_en0.txt")); i != "en0" || g != "192.0.2.1" {
		t.Errorf("route = %q %q", i, g)
	}
	if i, g := ParseRoute(fx(t, "route_not_in_table.txt")); i != "" || g != "" {
		t.Errorf("no route = %q %q", i, g)
	}
	if st, ip := ParseIfconfig(fx(t, "ifconfig_en0.txt")); st != "active" || ip != "192.0.2.83" {
		t.Errorf("ifconfig = %q %q", st, ip)
	}
	if on, err := ParseAirportPower(fx(t, "getairportpower_en0_on.txt")); err != nil || !on {
		t.Errorf("airport = %v %v", on, err)
	}
	if on, err := ParseAirportPower(fx(t, "synth_getairportpower_en0_off.txt")); err != nil || on {
		t.Errorf("airport off = %v %v", on, err)
	}
	if _, err := ParseAirportPower(fx(t, "getairportpower_en10_error.txt")); err == nil || !strings.Contains(err.Error(), "not a Wi-Fi interface") {
		t.Errorf("airport en10 err = %v", err)
	}
	if on, err := ParseServiceEnabled(fx(t, "getnetworkserviceenabled_iphone_enabled.txt")); err != nil || !on {
		t.Errorf("enabled = %v %v", on, err)
	}
	if _, err := ParseServiceEnabled(fx(t, "getnetworkserviceenabled_bogus_error.txt")); err == nil {
		t.Error("bogus service should fail")
	}
	if ParseIOUSBHasIPhone(fx(t, "ioreg_iousb_no_iphone.txt")) || !ParseIOUSBHasIPhone(fx(t, "synth_ioreg_iousb_iphone.txt")) {
		t.Error("IOUSB iPhone detection wrong")
	}
}

func TestSetServiceEnabled(t *testing.T) {
	set := cl(paths.BinNetworksetup, "-setnetworkserviceenabled", "iPhone USB", "off")
	get := cl(paths.BinNetworksetup, "-getnetworkserviceenabled", "iPhone USB")
	ex := fakes.NewExec().On(set, "").On(get, fx(t, "synth_getnetworkserviceenabled_disabled.txt"))
	if err := newNet(ex, Options{}, &stubProbe{}).SetServiceEnabled(context.Background(), "iPhone USB", false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ex.Calls(), []string{set, get}) {
		t.Errorf("calls = %q", ex.Calls())
	}
}

func TestSetServiceEnabledErrors(t *testing.T) {
	set := cl(paths.BinNetworksetup, "-setnetworkserviceenabled", "iPhone USBX", "on")
	get := cl(paths.BinNetworksetup, "-getnetworkserviceenabled", "iPhone USBX")
	// Error text on exit status 0 (PLAN: networksetup exits 0 on failure).
	ex := fakes.NewExec().On(set, fx(t, "synth_setter_error.txt"))
	err := newNet(ex, Options{}, &stubProbe{}).SetServiceEnabled(context.Background(), "iPhone USBX", true)
	if err == nil || !strings.Contains(err.Error(), "not a recognized network service") {
		t.Fatalf("exit 0 error text: err = %v", err)
	}
	if ex.CallCount(get) != 0 {
		t.Error("verified after a reported failure")
	}
	// Same text with the recorded non-zero status.
	ex = fakes.NewExec().OnResponse(set, fakes.ExecResponse{
		Result: contracts.ExecResult{Stdout: []byte(fx(t, "getnetworkserviceenabled_bogus_error.txt")), ExitCode: 4},
		Err:    &contracts.ExitError{Cmd: set, ExitCode: 4},
	})
	if err := newNet(ex, Options{}, &stubProbe{}).SetServiceEnabled(context.Background(), "iPhone USBX", true); err == nil || !strings.Contains(err.Error(), "parameters were not valid") {
		t.Fatalf("exit 4: err = %v", err)
	}
}

func TestSetServiceEnabledVerifyFails(t *testing.T) {
	set := cl(paths.BinNetworksetup, "-setnetworkserviceenabled", "iPhone USB", "off")
	get := cl(paths.BinNetworksetup, "-getnetworkserviceenabled", "iPhone USB")
	// Silent success, but the service stays enabled.
	ex := fakes.NewExec().On(set, "").On(get, fx(t, "getnetworkserviceenabled_iphone_enabled.txt"))
	err := newNet(ex, Options{}, &stubProbe{}).SetServiceEnabled(context.Background(), "iPhone USB", false)
	if err == nil || !strings.Contains(err.Error(), "not applied") {
		t.Fatalf("err = %v", err)
	}
	if n := ex.CallCount(get); n != verifyAttempts {
		t.Errorf("read back %d times, want %d", n, verifyAttempts)
	}
	// Lagging state: the second read-back succeeds.
	ex = fakes.NewExec().On(set, "").
		On(get, fx(t, "getnetworkserviceenabled_iphone_enabled.txt")).
		On(get, fx(t, "synth_getnetworkserviceenabled_disabled.txt"))
	if err := newNet(ex, Options{}, &stubProbe{}).SetServiceEnabled(context.Background(), "iPhone USB", false); err != nil {
		t.Fatalf("lagging verify: %v", err)
	}
}

// renderOrder produces -listnetworkserviceorder output for svcs.
func renderOrder(svcs []Service) string {
	var b strings.Builder
	b.WriteString("An asterisk (*) denotes that a network service is disabled.\n")
	for i, s := range svcs {
		idx := fmt.Sprint(i + 1)
		if !s.Enabled {
			idx = "*"
		}
		fmt.Fprintf(&b, "(%s) %s\n(Hardware Port: %s, Device: %s)\n\n", idx, s.Name, s.HardwarePort, s.Device)
	}
	return b.String()
}

func TestSetServiceOrder(t *testing.T) {
	orig, err := ParseServiceOrder(fx(t, "listnetworkserviceorder.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if renderOrder(orig) != fx(t, "listnetworkserviceorder.txt") {
		t.Fatal("renderOrder does not reproduce the recorded listing")
	}
	// Move iPhone USB ahead of Wi-Fi.
	moved := slices.Clone(orig)
	iphone := moved[5]
	moved = slices.Delete(moved, 5, 6)
	moved = slices.Insert(moved, 3, iphone)
	var order []string
	for _, s := range moved {
		order = append(order, s.Name)
	}
	set := cl(paths.BinNetworksetup, append([]string{"-ordernetworkservices"}, order...)...)

	ex := fakes.NewExec().
		On(cmdOrder, renderOrder(orig)).
		On(cmdOrder, renderOrder(moved)).
		On(set, "")
	if err := newNet(ex, Options{}, &stubProbe{}).SetServiceOrder(context.Background(), order); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ex.Calls(), []string{cmdOrder, set, cmdOrder}) {
		t.Errorf("calls = %q", ex.Calls())
	}

	// Not applied: the listing does not change.
	ex = fakes.NewExec().On(cmdOrder, renderOrder(orig)).On(set, "")
	if err := newNet(ex, Options{}, &stubProbe{}).SetServiceOrder(context.Background(), order); err == nil || !strings.Contains(err.Error(), "not applied") {
		t.Fatalf("err = %v", err)
	}

	// Not a permutation: rejected before running networksetup.
	ex = fakes.NewExec().On(cmdOrder, renderOrder(orig))
	if err := newNet(ex, Options{}, &stubProbe{}).SetServiceOrder(context.Background(), order[:3]); err == nil {
		t.Fatal("partial order accepted")
	}
	if len(ex.Calls()) != 1 {
		t.Errorf("calls = %q", ex.Calls())
	}
}

func TestWiFiPower(t *testing.T) {
	// Autodetected from the Wi-Fi hardware port.
	ex := fakes.NewExec().On(cmdPorts, fx(t, "listallhardwareports_no_iphone.txt")).On(cmdAirport, fx(t, "getairportpower_en0_on.txt"))
	on, err := newNet(ex, Options{}, &stubProbe{}).WiFiPower(context.Background())
	if err != nil || !on {
		t.Fatalf("WiFiPower = %v, %v", on, err)
	}
	// Recorded error: not a Wi-Fi interface (exit status 10).
	cmd := cl(paths.BinNetworksetup, "-getairportpower", "en10")
	ex = fakes.NewExec().OnResponse(cmd, fakes.ExecResponse{
		Result: contracts.ExecResult{Stdout: []byte(fx(t, "getairportpower_en10_error.txt")), ExitCode: 10},
		Err:    &contracts.ExitError{Cmd: cmd, ExitCode: 10},
	})
	if _, err := newNet(ex, Options{WiFiIface: "en10"}, &stubProbe{}).WiFiPower(context.Background()); err == nil || !strings.Contains(err.Error(), "not a Wi-Fi interface") {
		t.Fatalf("err = %v", err)
	}
	// No Wi-Fi port anywhere.
	ex = fakes.NewExec().On(cmdPorts, "Hardware Port: Thunderbolt 1\nDevice: en1\n").On(cmdOrder, renderOrder([]Service{{"X", "X", "en1", true}}))
	if _, err := newNet(ex, Options{}, &stubProbe{}).WiFiPower(context.Background()); err == nil {
		t.Fatal("want error without a Wi-Fi port")
	}
}

func TestSetWiFiPower(t *testing.T) {
	set := cl(paths.BinNetworksetup, "-setairportpower", "en0", "off")
	ex := fakes.NewExec().On(set, "").On(cmdAirport, fx(t, "synth_getairportpower_en0_off.txt"))
	if err := newNet(ex, Options{WiFiIface: "en0"}, &stubProbe{}).SetWiFiPower(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	ex = fakes.NewExec().On(set, "").On(cmdAirport, fx(t, "getairportpower_en0_on.txt"))
	if err := newNet(ex, Options{WiFiIface: "en0"}, &stubProbe{}).SetWiFiPower(context.Background(), false); err == nil {
		t.Fatal("want verify failure")
	}
}

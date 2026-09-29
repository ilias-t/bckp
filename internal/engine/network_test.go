package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
)

// netTok builds probes: on (online via en10, gateway ok), off (no route,
// iface present), gone (no route, iface missing), wifi (online via en0,
// hotspot gateway unreachable), err (probe failed), "+Ns" (extra gap before
// the next probe).
func netSeq(t *testing.T, spec string, wifiOn bool) []struct {
	in  NetInput
	gap time.Duration
} {
	t.Helper()
	type item = struct {
		in  NetInput
		gap time.Duration
	}
	var out []item
	var gap time.Duration
	for _, tok := range strings.Fields(spec) {
		var in NetInput
		switch {
		case tok == "on":
			in.Snap = contracts.NetSnapshot{DefaultIface: "en10", Iface: "en10", Online: true, IfacePresent: true, GatewayReachable: true, WiFiOn: wifiOn}
		case tok == "wifi":
			in.Snap = contracts.NetSnapshot{DefaultIface: "en0", Iface: "en10", Online: true, IfacePresent: true, WiFiOn: wifiOn}
		case tok == "off":
			in.Snap = contracts.NetSnapshot{Iface: "en10", IfacePresent: true, Diag: "route=none\nen10=yes"}
		case tok == "route-no-inet":
			in.Snap = contracts.NetSnapshot{DefaultIface: "en10", Iface: "en10", IfacePresent: true}
		case tok == "gone":
			in.Snap = contracts.NetSnapshot{Iface: "en10"}
		case tok == "err":
			in.Err = errors.New("probe failed")
		case strings.HasPrefix(tok, "+"):
			d, err := time.ParseDuration(tok[1:])
			if err != nil {
				t.Fatal(err)
			}
			gap = d
			continue
		case strings.HasPrefix(tok, "x"):
			n := 0
			for _, c := range tok[1:] {
				n = n*10 + int(c-'0')
			}
			for i := 1; i < n; i++ {
				out = append(out, item{in: out[len(out)-1].in})
			}
			continue
		default:
			t.Fatalf("bad token %q", tok)
		}
		out = append(out, item{in: in, gap: gap})
		gap = 0
	}
	return out
}

func runNet(t *testing.T, r networkRules, s NetFSM, spec string, wifiOn bool, start time.Time, recoverErr error) (NetFSM, []string, []Action) {
	t.Helper()
	var got []string
	var all []Action
	now := start
	for i, it := range netSeq(t, spec, wifiOn) {
		now = now.Add(it.gap)
		var acts []Action
		s, acts = r.Step(s, it.in, now)
		for _, a := range acts {
			if a.Kind == ActRecover {
				var more []Action
				s, more = r.RecoveryDone(s, recoverErr, now.Add(RecoveryToggleGap))
				acts = append(acts, more...)
			}
		}
		for _, a := range acts {
			all = append(all, a)
			if d := describe(i+1, a); d != "" {
				got = append(got, d)
			}
		}
		now = now.Add(poll)
	}
	return s, got, all
}

func TestNetworkStep(t *testing.T) {
	tests := []struct {
		name       string
		cfg        func(*config.Network)
		mode       contracts.NetworkMode
		spec       string
		wifiOn     bool
		recoverErr error
		want       []string
		online     bool
		recoveries int
	}{
		{name: "online stays quiet", spec: "on x5", online: true},
		{
			// Offline 15s..90s.
			name: "short outage: no recovery, back-online notice", spec: "on off x5 on",
			want: []string{"7:p3:Mac back online after 1m15s via hotspot (USB)"}, online: true,
		},
		{
			name: "default route without internet is offline", spec: "on route-no-inet x2 on",
			want: []string{"4:p3:Mac back online after 30s via hotspot (USB)"}, online: true,
		},
		{
			// Offline from poll 2 (t=15s); 90s later is poll 8.
			name: "recovery after 90s offline, then at most once per 180s",
			spec: "on off x20", want: []string{"8:recover", "20:recover"}, recoveries: 2,
		},
		{
			name: "no recovery when the hotspot interface is missing", spec: "on gone x20",
		},
		{
			name: "no recovery when the watchdog is disabled", cfg: func(c *config.Network) { c.Watchdog = false },
			spec: "on off x20",
		},
		{
			name: "probe errors are neither online nor offline", spec: "on err x10 on", online: true,
		},
		{
			name: "outage across a probe error", spec: "on off err off x2 on",
			want: []string{"6:p3:Mac back online after 1m via hotspot (USB)"}, online: true,
		},
		{
			name: "prefer hotspot never touches wifi", mode: contracts.NetworkPreferHotspot,
			wifiOn: true, spec: "on x5", online: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mode := tc.mode
			if mode == "" {
				mode = contracts.NetworkWatchdog
			}
			r := networkRules{Cfg: config.Default().Network, Poll: poll, Mode: mode}
			if tc.cfg != nil {
				tc.cfg(&r.Cfg)
			}
			s, got, _ := runNet(t, r, NetFSM{}, tc.spec, tc.wifiOn, t0, tc.recoverErr)
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("actions:\n got %q\nwant %q", got, tc.want)
			}
			if s.Online != tc.online {
				t.Errorf("online = %v, want %v", s.Online, tc.online)
			}
			if s.Recoveries != tc.recoveries {
				t.Errorf("recoveries = %d, want %d", s.Recoveries, tc.recoveries)
			}
		})
	}
}

func TestNetworkBackOnlineBody(t *testing.T) {
	r := networkRules{Cfg: config.Default().Network, Poll: poll, Mode: contracts.NetworkWatchdog}
	// Offline 12:00:15; recovery at 12:01:45; a 5m gap (Mac asleep?) makes
	// a second attempt due at 12:07; back online after that.
	_, _, acts := runNet(t, r, NetFSM{}, "on off x7 +5m off on", false, t0, nil)
	var body, title string
	for _, a := range acts {
		if a.Kind == ActNotify {
			title, body = a.Notification.Title, a.Notification.Body
		}
	}
	if title != "Mac back online after 7m via hotspot (USB)" {
		t.Fatalf("title = %q", title)
	}
	for _, want := range []string{
		"Offline 12:00-12:07 (7m).",
		"12:00 first: route=none; en10=yes",
		"12:07 latest: route=none; en10=yes",
		"12:01 recovery: toggled \"iPhone USB\" off/on ok",
		"note: no polls for 5m15s (Mac asleep?)",
		"12:07 recovery: toggled \"iPhone USB\" off/on ok",
		"Recovery attempts: 2",
		"now: route=en10",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

func TestNetworkGapLogged(t *testing.T) {
	r := networkRules{Cfg: config.Default().Network, Poll: poll, Mode: contracts.NetworkWatchdog}
	_, _, acts := runNet(t, r, NetFSM{}, "on +46s on", false, t0, nil)
	found := false
	for _, a := range acts {
		if a.Kind == ActLog && strings.Contains(a.Msg, "Mac asleep?") {
			found = true
		}
	}
	if !found {
		t.Fatal("a gap > 3 polls must be logged")
	}
	_, _, acts = runNet(t, r, NetFSM{}, "on +30s on", false, t0, nil) // 45s gap: not > 3 polls
	for _, a := range acts {
		if a.Kind == ActLog && strings.Contains(a.Msg, "Mac asleep?") {
			t.Fatal("gap of exactly 3 polls must not be logged")
		}
	}
}

// TestRecoveryCountedWhenRun: a recovery counts once its toggle ran
// (RecoveryDone), not when the state machine asks for it; the engine may
// refuse to run it.
func TestRecoveryCountedWhenRun(t *testing.T) {
	r := networkRules{Cfg: config.Default().Network, Poll: poll, Mode: contracts.NetworkWatchdog}
	s := NetFSM{}
	now := t0
	asked := false
	for _, it := range netSeq(t, "on off x7", false) {
		var acts []Action
		s, acts = r.Step(s, it.in, now)
		for _, a := range acts {
			asked = asked || a.Kind == ActRecover
		}
		now = now.Add(poll)
	}
	if !asked || s.Recoveries != 0 || s.OutageRecoveries != 0 {
		t.Fatalf("asked %v, recoveries %d/%d before the toggle ran", asked, s.Recoveries, s.OutageRecoveries)
	}
	s, _ = r.RecoveryDone(s, errors.New("networksetup: error"), now)
	if s.Recoveries != 1 || s.OutageRecoveries != 1 {
		t.Fatalf("recoveries %d/%d after the toggle ran", s.Recoveries, s.OutageRecoveries)
	}
	// A toggle that finishes after the link came back counts for the
	// session only.
	s, _, _ = runNet(t, r, s, "on", false, now, nil)
	s, _ = r.RecoveryDone(s, nil, now.Add(poll))
	if s.Recoveries != 2 || s.OutageRecoveries != 0 {
		t.Fatalf("recoveries %d/%d after going online", s.Recoveries, s.OutageRecoveries)
	}
}

func TestDiagLine(t *testing.T) {
	got := diagLine(contracts.NetSnapshot{Iface: "en10", IfacePresent: true, IfaceIPv4: "172.20.10.2", WiFiOn: true})
	want := "route=none en10=yes ipv4=172.20.10.2 ping_gw=fail iphone_usb=no wifi=on online=no"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if got := diagLine(contracts.NetSnapshot{Diag: "a\n  b\n"}); got != "a; b" {
		t.Fatalf("multi-line diag = %q", got)
	}
}

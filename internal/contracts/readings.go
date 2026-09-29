package contracts

import (
	"fmt"
	"strings"
	"time"
)

// ThermalState mirrors ProcessInfo.thermalState. The numeric order is
// meaningful: Nominal < Fair < Serious < Critical, so comparisons such as
// `s >= ThermalSerious` work.
//
// The zero value is ThermalUnknown, so a zero ThermalReading never looks
// like a healthy "nominal" reading. Consumers must treat ThermalUnknown as a
// failed reading, not as "cool" (it sorts below ThermalNominal).
type ThermalState int

const (
	ThermalUnknown ThermalState = iota
	ThermalNominal
	ThermalFair
	ThermalSerious
	ThermalCritical
)

var thermalStateNames = [...]string{
	ThermalUnknown:  "unknown",
	ThermalNominal:  "nominal",
	ThermalFair:     "fair",
	ThermalSerious:  "serious",
	ThermalCritical: "critical",
}

// String returns the lower-case name ("nominal", "fair", "serious",
// "critical", or "unknown"). The same names are used on the wire and in the
// `backpack simulate` script grammar.
func (s ThermalState) String() string {
	if s < 0 || int(s) >= len(thermalStateNames) {
		return fmt.Sprintf("ThermalState(%d)", int(s))
	}
	return thermalStateNames[s]
}

// ParseThermalState parses a state name case-insensitively. It accepts the
// names produced by String, including "unknown".
func ParseThermalState(name string) (ThermalState, error) {
	lower := strings.ToLower(strings.TrimSpace(name))
	for i, n := range thermalStateNames {
		if n == lower {
			return ThermalState(i), nil
		}
	}
	return ThermalUnknown, fmt.Errorf("contracts: unknown thermal state %q", name)
}

// MarshalText encodes the state as its lower-case name.
func (s ThermalState) MarshalText() ([]byte, error) {
	if s < 0 || int(s) >= len(thermalStateNames) {
		return nil, fmt.Errorf("contracts: invalid thermal state %d", int(s))
	}
	return []byte(s.String()), nil
}

// UnmarshalText decodes a state name case-insensitively.
func (s *ThermalState) UnmarshalText(b []byte) error {
	v, err := ParseThermalState(string(b))
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// ThermalReading is one thermal poll.
//
// State comes from the cgo ProcessInfo.thermalState shim
// (providers/thermal); CPUC/GPUC come from macmon (providers/macmon).
// providers/thermal is a combining ThermalSource that fills all three. Temperatures are
// advisory: nil means unavailable (macmon missing, failed, or implausible).
// The "ignore readings below 15°C" filter (idle GPU reads ~4°C) is the
// provider's job; consumers never see such values.
type ThermalReading struct {
	State ThermalState `json:"state"`
	CPUC  *float64     `json:"cpu_c,omitempty"`
	GPUC  *float64     `json:"gpu_c,omitempty"`
	At    time.Time    `json:"at"`
}

// PowerReading is one power poll (pmset -g batt plus ioreg
// AppleClamshellState / SleepDisabled).
type PowerReading struct {
	// OnAC is true when running from an external power source.
	OnAC bool `json:"on_ac"`
	// Percent is the internal battery charge, 0–100.
	Percent int `json:"percent"`
	// Charging is true when the battery is actively charging.
	Charging bool `json:"charging"`
	// LidClosed reports AppleClamshellState.
	LidClosed bool `json:"lid_closed"`
	// SleepDisabled reports the kernel's SleepDisabled flag (pmset
	// disablesleep), read from ioreg. This is the ground truth for whether
	// the lid-close sleep is currently blocked.
	SleepDisabled bool      `json:"sleep_disabled"`
	At            time.Time `json:"at"`
}

// NetSnapshot is one network poll. "Hotspot interface" means the BSD
// interface backing the configured hotspot service (config
// network.service, default "iPhone USB"; e.g. en10), or config
// network.iface when set.
type NetSnapshot struct {
	// DefaultIface is the interface of the current default route ("" when
	// there is no default route).
	DefaultIface string `json:"default_iface"`
	// Gateway is the hotspot gateway address (config network.gateway, or
	// the router discovered for the hotspot interface). "" when unknown.
	Gateway string `json:"gateway"`
	// Online is true when end-to-end internet reachability succeeded.
	Online bool `json:"online"`
	// Iface is the hotspot interface name ("" when it cannot be resolved).
	Iface string `json:"iface"`
	// IfacePresent is true when the hotspot interface exists (ifconfig).
	IfacePresent bool `json:"iface_present"`
	// IfaceIPv4 is the hotspot interface's IPv4 address ("" when none).
	IfaceIPv4 string `json:"iface_ipv4"`
	// GatewayReachable is true when the hotspot gateway answered a probe.
	GatewayReachable bool `json:"gateway_reachable"`
	// IPhoneUSBPresent is true when the hotspot network service exists and
	// its hardware port is present (networksetup).
	IPhoneUSBPresent bool `json:"iphone_usb_present"`
	// WiFiOn reports Wi-Fi power.
	WiFiOn bool `json:"wifi_on"`
	// WiFiIface is the Wi-Fi interface (e.g. en0; "" when unknown).
	WiFiIface string `json:"wifi_iface,omitempty"`
	// DefaultGateway is the default route's gateway ("" when there is no
	// default route or it names none).
	DefaultGateway string `json:"default_gateway,omitempty"`
	// Checks holds the DNS/IP checks, run only when the snapshot is not
	// Online (nil otherwise).
	Checks *NetChecks `json:"checks,omitempty"`
	// Diag is a human-readable, multi-line diagnostic summary suitable for
	// queuing into an outage notification.
	Diag string    `json:"diag,omitempty"`
	At   time.Time `json:"at"`
}

// Results of one NetChecks check.
const (
	CheckOK   = "ok"
	CheckFail = "fail"
	// CheckSkip: not run (no time left in the snapshot's deadline).
	CheckSkip = "skip"
)

// NetChecks tell a dead link from a broken resolver when the reachability
// probe fails: a VPN such as Tailscale can own the system resolver, so an
// "outage" may be its DNS alone. Each field is CheckOK, CheckFail or
// CheckSkip.
type NetChecks struct {
	// DNSSys: the probe host resolves through the system resolver.
	DNSSys string `json:"dns_sys"`
	// DNSDirect: the probe host resolves through a public resolver
	// (1.1.1.1), bypassing the system's.
	DNSDirect string `json:"dns_direct"`
	// IPOK: a public IP (1.1.1.1) answers a ping.
	IPOK string `json:"ip_ok"`
}

// Verdict reads the checks: "", or a short hint at the likely cause.
func (c NetChecks) Verdict() string {
	switch {
	case c.IPOK == CheckFail:
		return "no IP connectivity"
	case c.IPOK == CheckOK && c.DNSSys == CheckFail && c.DNSDirect == CheckOK:
		return "system DNS failing, direct DNS works (VPN/Tailscale DNS?)"
	case c.IPOK == CheckOK && c.DNSSys == CheckFail && c.DNSDirect == CheckFail:
		return "IP works, DNS failing"
	case c.IPOK == CheckOK && c.DNSSys == CheckOK:
		return "IP and DNS work (HTTP probe blocked or captive portal?)"
	}
	return ""
}

// HotspotGateway is the gateway iOS Personal Hotspot always uses, over USB
// and over Wi-Fi alike.
const HotspotGateway = "172.20.10.1"

// NetLink is how the Mac reaches the network: which interface carries the
// default route, and whether it is the iPhone hotspot.
type NetLink string

const (
	LinkNone        NetLink = "none"         // no default route
	LinkHotspotUSB  NetLink = "hotspot_usb"  // the hotspot service's interface (iPhone USB)
	LinkHotspotWiFi NetLink = "hotspot_wifi" // Wi-Fi, gateway HotspotGateway
	LinkWiFi        NetLink = "wifi"         // Wi-Fi, any other network
	LinkOther       NetLink = "other"        // any other interface (Ethernet, VPN, ...)
)

// Label renders the link for humans: "hotspot (USB)", "hotspot (Wi-Fi)",
// "Wi-Fi", "other" or "none".
func (l NetLink) Label() string {
	switch l {
	case LinkHotspotUSB:
		return "hotspot (USB)"
	case LinkHotspotWiFi:
		return "hotspot (Wi-Fi)"
	case LinkWiFi:
		return "Wi-Fi"
	case LinkOther:
		return "other"
	case LinkNone, "":
		return "none"
	}
	return string(l)
}

// Link classifies the default route. Over Wi-Fi, the hotspot is told
// apart from other networks by its fixed gateway (HotspotGateway): SSIDs
// are redacted for command-line tools without Location Services. The Wi-Fi
// cases need WiFiIface; the gateway alone is not enough, since the USB
// link uses the same one.
func (n NetSnapshot) Link() NetLink {
	switch {
	case n.DefaultIface == "":
		return LinkNone
	case n.Iface != "" && n.DefaultIface == n.Iface:
		return LinkHotspotUSB
	case n.WiFiIface != "" && n.DefaultIface == n.WiFiIface && n.DefaultGateway == HotspotGateway:
		return LinkHotspotWiFi
	case n.WiFiIface != "" && n.DefaultIface == n.WiFiIface:
		return LinkWiFi
	}
	return LinkOther
}

// Priority levels for Notification (ntfy semantics).
const (
	PriorityMin     = 1
	PriorityLow     = 2
	PriorityDefault = 3
	PriorityHigh    = 4
	PriorityUrgent  = 5
)

// NotifyClass says whether a notification needs the user's attention. The
// engine decides it where the notification is made and pushes only
// ClassAttention unless config notify.push = "all".
type NotifyClass string

const (
	// ClassAttention: warnings, errors, and things that need the user
	// (heat, battery, lost lease, an end the user did not ask for, a long
	// outage). Always pushed. "" means ClassAttention.
	ClassAttention NotifyClass = "attention"
	// ClassInfo: the happy path (start, requested stop, heartbeat, short
	// outages). Logged and listed in the TUI; pushed only with
	// notify.push = "all".
	ClassInfo NotifyClass = "info"
)

// Notification is a message for the user's phone or desktop.
type Notification struct {
	// Priority is 1–5 (PriorityMin..PriorityUrgent). Zero means
	// PriorityDefault.
	Priority int      `json:"priority"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	Tags     []string `json:"tags,omitempty"`
	// Class is set by the engine; notifiers ignore it.
	Class NotifyClass `json:"class,omitempty"`
}

// Validate reports whether n is sendable.
func (n Notification) Validate() error {
	if n.Priority != 0 && (n.Priority < PriorityMin || n.Priority > PriorityUrgent) {
		return fmt.Errorf("contracts: notification priority %d out of range 1-5", n.Priority)
	}
	if n.Title == "" && n.Body == "" {
		return fmt.Errorf("contracts: notification has neither title nor body")
	}
	return nil
}

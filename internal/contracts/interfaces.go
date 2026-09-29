package contracts

import (
	"context"
	"errors"
	"time"
)

// ThermalSource reads the machine's thermal state. See ThermalReading for
// how the cgo state shim and macmon temperatures combine.
//
// Read returns an error only when State could not be determined; missing
// temperatures are reported as nil CPUC/GPUC, not as an error.
type ThermalSource interface {
	Read(ctx context.Context) (ThermalReading, error)
}

// PowerSource reads battery, AC, lid and SleepDisabled state.
type PowerSource interface {
	Read(ctx context.Context) (PowerReading, error)
}

// Network observes and adjusts macOS network services without root
// (the user must be in the admin group for the setters).
//
// Setters shell out to networksetup, which exits 0 even on failure, so
// implementations must parse its output and return an error on failure.
type Network interface {
	// Snapshot probes current connectivity. It returns an error only when
	// probing itself failed; being offline is reported in the snapshot.
	Snapshot(ctx context.Context) (NetSnapshot, error)
	// ServiceOrder returns network service names in priority order
	// (networksetup -listnetworkserviceorder), without the leading "*"
	// that marks disabled services.
	ServiceOrder(ctx context.Context) ([]string, error)
	// SetServiceOrder applies a full service order
	// (networksetup -ordernetworkservices). The slice must contain every
	// service returned by ServiceOrder.
	SetServiceOrder(ctx context.Context, order []string) error
	// SetServiceEnabled enables or disables one service
	// (networksetup -setnetworkserviceenabled).
	SetServiceEnabled(ctx context.Context, service string, on bool) error
	// WiFiPower reports Wi-Fi radio power (networksetup -getairportpower).
	WiFiPower(ctx context.Context) (bool, error)
	// SetWiFiPower turns the Wi-Fi radio on or off
	// (networksetup -setairportpower).
	SetWiFiPower(ctx context.Context, on bool) error
}

// Notifier delivers a Notification. Send returns only after the backend
// accepted the message (e.g. ntfy answered HTTP 2xx) or failed. Durable
// retry is the outbox's job (notify/outbox wraps another Notifier), not the
// backend's.
type Notifier interface {
	Send(ctx context.Context, n Notification) error
}

// Outbox is optionally implemented by a Notifier that keeps failed sends
// on disk for retry (internal/notify/outbox, or a chain containing one).
// Its Send wraps ErrQueued when a message was kept.
type Outbox interface {
	// Pending returns the number of messages waiting.
	Pending() int
	// Flush retries them, oldest first.
	Flush(ctx context.Context) error
}

// SleepGuard is the engine's view of backpackd: it keeps the lid-closed Mac
// awake while a Lease is held.
//
// Lease rules (enforced by the daemon, see internal/proto):
//   - At most one lease exists system-wide. Acquire fails with ErrLeaseBusy
//     while another lease is active.
//   - A lease is bound to the connection that acquired it. If that
//     connection closes, the daemon releases the lease immediately.
//   - The TTL is clamped to 30–300s; the holder renews every ~TTL/3.
type SleepGuard interface {
	// Acquire opens a lease connection, sends `acquire`, and returns the
	// lease. reason is informational (logged by the daemon, truncated to
	// 128 chars).
	Acquire(ctx context.Context, ttl time.Duration, reason string) (Lease, error)
	// SleepNow puts the Mac to sleep via the daemon's `sleep` op
	// (disablesleep 0 -> verify via ioreg -> pmset sleepnow).
	//
	// If this guard holds a lease, the op is sent on that lease's
	// connection with its id, and the lease ends: its Lost channel closes
	// without a value, as after Release. If no lease is held, the op is
	// sent on a fresh connection without a lease id; the daemon accepts
	// that only when no lease is active anywhere (else ErrLeaseBusy).
	SleepNow(ctx context.Context, reason string) error
	// Status queries the daemon on a fresh short-lived connection.
	Status(ctx context.Context) (GuardStatus, error)
}

// Lease is a held sleep-disable lease.
type Lease interface {
	// ID is the daemon-assigned opaque lease id.
	ID() string
	// Expires is the current expiry as last reported by the daemon.
	Expires() time.Time
	// Renew extends the lease by its original TTL. After the lease is lost
	// or released it returns an error matching ErrLeaseLost.
	Renew(ctx context.Context) error
	// Release ends the lease (the daemon runs disablesleep 0). Releasing
	// an already-ended lease returns nil.
	Release(ctx context.Context) error
	// Lost returns a channel that:
	//   - receives exactly one non-nil error (matching ErrLeaseLost) and is
	//     then closed, when the lease ends for any reason other than
	//     Release or SleepNow: connection dropped, daemon restarted, renew
	//     rejected, TTL expired, or a daemon backstop fired;
	//   - is closed without a value after a successful Release or SleepNow.
	// The same channel is returned on every call.
	Lost() <-chan error
}

// GuardStatus is the daemon's `status` answer.
type GuardStatus struct {
	// SleepDisabled is the kernel's SleepDisabled flag as the daemon last
	// read it from ioreg.
	SleepDisabled bool `json:"sleep_disabled"`
	// Lease is the active lease, or nil.
	Lease *LeaseInfo `json:"lease"`
	// Backstop describes the daemon's hard-coded backstop: BackstopIdle,
	// BackstopArmed, or BackstopTrippedPrefix + reason.
	Backstop string `json:"backstop"`
}

// Backstop values reported in GuardStatus.Backstop.
const (
	// BackstopIdle: no lease is held; the backstop is not polling.
	BackstopIdle = "idle"
	// BackstopArmed: a lease is held and the backstop polls every 10s.
	BackstopArmed = "armed"
	// BackstopTrippedPrefix prefixes the reason of the most recent trip,
	// e.g. "tripped: thermal critical x2" or "tripped: battery 9%". It is
	// reported until the next successful acquire.
	BackstopTrippedPrefix = "tripped: "
)

// LeaseInfo describes the active lease in a daemon status answer.
type LeaseInfo struct {
	ID         string    `json:"id"`
	Reason     string    `json:"reason"`
	UID        int       `json:"uid"`
	PID        int       `json:"pid,omitempty"`
	TTLS       int       `json:"ttl_s"`
	AcquiredAt time.Time `json:"acquired_at"`
	Expires    time.Time `json:"expires"`
}

// Sentinel errors shared across the daemon client, engine and CLI. Wrap
// them with fmt.Errorf("...: %w", ErrX) and test with errors.Is.
var (
	// ErrLeaseBusy: another lease is active.
	ErrLeaseBusy = errors.New("sleep lease busy: another lease is active")
	// ErrLeaseLost: the lease no longer exists (released, expired, dropped
	// connection, wrong id/connection, or a backstop fired).
	ErrLeaseLost = errors.New("sleep lease lost")
	// ErrNotAllowed: the daemon rejected the caller's uid.
	ErrNotAllowed = errors.New("caller not allowed by backpackd")
	// ErrDaemonUnavailable: the daemon socket could not be reached
	// (not installed, not running, or refusing connections).
	ErrDaemonUnavailable = errors.New("backpackd unavailable")
	// ErrEngineNotRunning: no engine is listening on the engine socket.
	ErrEngineNotRunning = errors.New("backpack engine not running")
	// ErrNotAbortable: a critical-triggered sleep is pending and cannot be
	// aborted or postponed.
	ErrNotAbortable = errors.New("pending sleep cannot be aborted")
	// ErrNoSleepPending: AbortSleep was called with no sleep pending.
	ErrNoSleepPending = errors.New("no sleep pending")
	// ErrQueued: a Notifier could not deliver the message now and kept it
	// for retry (see Outbox).
	ErrQueued = errors.New("delivery failed, queued for retry")
	// ErrNoBattery: a PowerSource found no internal battery (a desktop
	// Mac).
	ErrNoBattery = errors.New("no internal battery")
	// ErrBadScript: a `simulate` script does not parse (see
	// simulate.Grammar).
	ErrBadScript = errors.New("bad script")
)

// Clock abstracts time so state machines and loops can be driven by a fake.
type Clock interface {
	Now() time.Time
	// After behaves like time.After.
	After(d time.Duration) <-chan time.Time
}

// EngineClient is the engine control surface used by the CLI and the TUI.
// internal/engineclient implements it over the engine socket
// (internal/proto); the TUI is built against it and tested with
// fakes.EngineClient.
type EngineClient interface {
	// Status returns the current snapshot. It returns an error matching
	// ErrEngineNotRunning when nothing listens on the engine socket.
	Status(ctx context.Context) (Snapshot, error)
	// Stop ends the session. sleep is tri-state; see proto.EngineRequest.
	Stop(ctx context.Context, sleep *bool) error
	// Extend adds minutes, or switches to forever when forever is true
	// (minutes is then ignored). It returns the updated snapshot.
	Extend(ctx context.Context, minutes int, forever bool) (Snapshot, error)
	// AbortSleep cancels a pending abortable sleep without adding time or
	// resetting the max_duration cap. It returns the updated snapshot, an
	// error matching ErrNotAbortable while a critical sleep is pending, or
	// one matching ErrNoSleepPending when nothing is pending.
	AbortSleep(ctx context.Context) (Snapshot, error)
	// Subscribe streams snapshots until the session ends, ctx is
	// cancelled, or the connection drops; then the channel is closed. The
	// first value is the current snapshot. If the session ended cleanly,
	// the last value has Session.Ended set; a close without such a value
	// means the connection was lost.
	Subscribe(ctx context.Context) (<-chan Snapshot, error)
}

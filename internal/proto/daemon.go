package proto

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ilias-t/bckp/internal/contracts"
)

// Daemon protocol (engine -> backpackd over paths.DaemonSocket).
//
// Transport and framing:
//   - Unix stream socket; one JSON object per line in each direction. The
//     daemon answers every request line with exactly one response line, in
//     order. A connection may carry any number of requests.
//   - On accept the daemon checks the peer uid with LOCAL_PEERCRED against
//     allowed_uids. A disallowed peer gets one Result{code: not_allowed}
//     line and the connection is closed.
//   - At most MaxDaemonConns connections are served at once; an extra
//     connection gets one Result{code: busy} line and is closed.
//   - A request line longer than MaxDaemonRequestBytes gets
//     Result{code: too_large} and the connection is closed. A line that is
//     not valid JSON, has unknown fields, or has trailing data gets
//     Result{code: bad_request} and the connection is closed (decode with
//     LineReader.Decode(v, strict=true)). Validation failures from
//     DaemonRequest.Validate get Result{code: bad_request} and the
//     connection stays open.
//
// Lease semantics:
//   - At most one lease exists. It is bound to the connection that
//     acquired it ("the lease connection").
//   - acquire: fails with busy if any lease is active (including one held
//     by the same connection). Otherwise runs `pmset -a disablesleep 1`,
//     creates the lease, and answers DaemonLeaseResponse. On pmset failure:
//     internal, no lease.
//   - renew: needs an active lease, the matching id, and the lease
//     connection. No lease -> no_lease; wrong id or wrong connection ->
//     lease_mismatch. Sets expires = now + the lease's TTL and answers
//     DaemonLeaseResponse.
//   - release: same checks as renew. Runs `pmset -a disablesleep 0`, ends
//     the lease, answers Result. The connection stays open.
//   - sleep: if a lease is active, same checks as renew (a sleep without a
//     lease id while a lease is active -> busy). If no lease is active, the
//     request must not carry a lease id (else no_lease) and may arrive on
//     any allowed connection. The daemon then runs `pmset -a disablesleep
//     0`, verifies SleepDisabled=No via ioreg, ends the lease if any,
//     writes Result{ok:true}, and only then runs `pmset sleepnow`. If
//     verification fails it answers internal and does not run sleepnow.
//   - status: any allowed connection, with or without a lease; never
//     changes state. Answers DaemonStatusResponse.
//   - When the lease connection closes, or the lease passes its expiry
//     without a renew, the daemon runs `pmset -a disablesleep 0`
//     immediately and ends the lease; on expiry it also closes the lease
//     connection so the client observes the loss.
//   - Backstop: while a lease is active the daemon polls every
//     BackstopPollInterval. BackstopCriticalPolls consecutive critical
//     thermal readings, BackstopBatteryTempPolls consecutive battery
//     readings at or above BackstopBatteryTempC, or internal battery below
//     BackstopBatteryPercent while not on AC, ends the lease (disablesleep 0), closes the lease
//     connection, and runs `pmset sleepnow`. Not configurable.
//   - On start and on SIGTERM the daemon runs `pmset -a disablesleep 0`.

// DaemonOp names a daemon operation.
type DaemonOp string

const (
	OpAcquire DaemonOp = "acquire"
	OpRenew   DaemonOp = "renew"
	OpRelease DaemonOp = "release"
	OpSleep   DaemonOp = "sleep"
	OpStatus  DaemonOp = "status"
)

// Daemon limits. Hard-coded; not configurable.
const (
	// DaemonMinTTL and DaemonMaxTTL bound acquire's ttl_s (clamped).
	DaemonMinTTL = 30 * time.Second
	DaemonMaxTTL = 300 * time.Second
	// DaemonDefaultTTL is used when acquire omits ttl_s; it is also the
	// engine's lease TTL.
	DaemonDefaultTTL = 90 * time.Second
	// RenewInterval is how often the engine renews a DaemonDefaultTTL
	// lease.
	RenewInterval = 30 * time.Second
	// MaxReasonLen caps reason in runes; longer reasons are truncated.
	MaxReasonLen = 128
	// MaxDaemonRequestBytes caps one request line (excluding '\n').
	MaxDaemonRequestBytes = 4 * 1024
	// MaxDaemonResponseBytes caps one response line for clients.
	MaxDaemonResponseBytes = 16 * 1024
	// MaxDaemonConns caps concurrently served connections.
	MaxDaemonConns = 8

	// BackstopPollInterval, BackstopCriticalPolls and
	// BackstopBatteryPercent define the daemon's own sleep backstop.
	BackstopPollInterval   = 10 * time.Second
	BackstopCriticalPolls  = 2
	BackstopBatteryPercent = 10
	// BackstopBatteryTempC and BackstopBatteryTempPolls: battery cell °C
	// the daemon never lets a session run past, whatever the engine's
	// battery_critical_c.
	BackstopBatteryTempC     = 50.0
	BackstopBatteryTempPolls = 2
)

// DaemonRequest is one request line. Only the fields listed per op are
// allowed; Validate rejects the rest.
//
//	{"op":"acquire","ttl_s":90,"reason":"..."}   ttl_s optional, reason optional
//	{"op":"renew","lease":"id"}
//	{"op":"release","lease":"id"}
//	{"op":"sleep","lease":"id","reason":"thermal critical"}   lease optional (see above)
//	{"op":"status"}
type DaemonRequest struct {
	Op     DaemonOp `json:"op"`
	TTLS   int      `json:"ttl_s,omitempty"`
	Reason string   `json:"reason,omitempty"`
	Lease  string   `json:"lease,omitempty"`
}

// Validate checks the request for its op and normalizes it in place:
// ttl_s is defaulted and clamped to [DaemonMinTTL, DaemonMaxTTL] and
// reason is sanitized with SanitizeReason. It returns a *RemoteError with
// CodeBadRequest on failure.
func (r *DaemonRequest) Validate() error {
	bad := func(format string, args ...any) error {
		return &RemoteError{Code: CodeBadRequest, Message: fmt.Sprintf(format, args...)}
	}
	switch r.Op {
	case OpAcquire:
		if r.Lease != "" {
			return bad("acquire takes no lease")
		}
		if r.TTLS < 0 {
			return bad("ttl_s must not be negative")
		}
		r.TTLS = int(ClampTTL(time.Duration(r.TTLS) * time.Second).Seconds())
	case OpRenew, OpRelease:
		if r.Lease == "" {
			return bad("%s needs a lease", r.Op)
		}
		if r.TTLS != 0 || r.Reason != "" {
			return bad("%s takes only a lease", r.Op)
		}
	case OpSleep:
		if r.TTLS != 0 {
			return bad("sleep takes no ttl_s")
		}
	case OpStatus:
		if r.TTLS != 0 || r.Reason != "" || r.Lease != "" {
			return bad("status takes no arguments")
		}
	case "":
		return bad("missing op")
	default:
		return bad("unknown op %q", r.Op)
	}
	r.Reason = SanitizeReason(r.Reason)
	return nil
}

// ClampTTL clamps d to [DaemonMinTTL, DaemonMaxTTL]; zero or negative
// becomes DaemonDefaultTTL.
func ClampTTL(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return DaemonDefaultTTL
	case d < DaemonMinTTL:
		return DaemonMinTTL
	case d > DaemonMaxTTL:
		return DaemonMaxTTL
	}
	return d
}

// SanitizeReason replaces control characters (including newlines) and
// invalid UTF-8 with spaces, trims surrounding space, and truncates to
// MaxReasonLen runes. Reasons are only ever logged.
func SanitizeReason(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == MaxReasonLen {
			break
		}
		if r == utf8.RuneError || unicode.IsControl(r) {
			r = ' '
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// DaemonLeaseResponse answers acquire and renew.
//
//	{"ok":true,"lease":"id","expires":"2026-09-28T12:00:00Z"}
type DaemonLeaseResponse struct {
	Result
	Lease   string    `json:"lease,omitempty"`
	Expires time.Time `json:"expires,omitzero"`
}

// DaemonStatusResponse answers status. GuardStatus's fields are flattened
// into the top level:
//
//	{"ok":true,"sleep_disabled":false,"lease":null,"backstop":"idle"}
//
// Note that "lease" is an object (contracts.LeaseInfo) or null here, while
// in DaemonLeaseResponse it is the id string. Clients always know which op
// they sent and decode into the matching type.
type DaemonStatusResponse struct {
	Result
	contracts.GuardStatus
}

// release and sleep are answered with a bare Result.

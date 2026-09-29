package proto

import (
	"fmt"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
)

// Engine protocol (CLI/TUI -> engine over paths.User.EngineSocket).
//
// Transport and framing:
//   - Unix stream socket, mode 0600, in the user's Application Support
//     directory; no peer check beyond file permissions.
//   - One request per connection. The client writes one EngineRequest line;
//     the engine writes one EngineResponse line and closes, except for
//     subscribe (below). Requests are capped at MaxEngineRequestBytes;
//     decode leniently (strict=false).
//
// Ops:
//   - status: {ok, snapshot}.
//   - stop{sleep}: ends the session; the engine releases its lease,
//     restores the network, and exits after answering {ok, snapshot} (the
//     snapshot has Session.Ended set). sleep is tri-state:
//   - sleep=nil: end the session without an explicit sleep. A pending
//     critical sleep still executes first (EndThermalSleep); a pending
//     abortable sleep is cancelled (EndStopped).
//   - sleep=true: end the session, then call the daemon's sleep op
//     (EndUserSleep).
//   - sleep=false: end the session without sleeping; cancels a pending
//     abortable sleep (EndStopped). If a critical sleep is pending,
//     answers not_abortable and changes nothing.
//   - extend{minutes|forever}: exactly one of minutes (1..MaxExtendMinutes)
//     or forever=true. minutes adds to the requested end (from now if the
//     end has passed; a forever session stays forever). forever switches
//     the session to forever. Both reset CapAt to now + max_duration,
//     clear CapWarned, and cancel a pending abortable sleep. With a
//     critical sleep pending, answers not_abortable and changes nothing.
//     Answers {ok, snapshot}.
//   - abort_sleep: cancels a pending abortable sleep and nothing else: the
//     requested end, CapAt and CapWarned are unchanged. Answers {ok,
//     snapshot}; not_abortable while a critical sleep is pending;
//     no_sleep_pending when no sleep is pending.
//   - subscribe: the engine writes the current snapshot immediately, then
//     one per state change and at least once per poll interval, each as
//     an EngineResponse{ok, snapshot}. When the session ends the last line
//     has Done=true and a snapshot with Session.Ended set, then the engine
//     closes. The client unsubscribes by closing the connection. A
//     subscriber whose writes block longer than SubscribeWriteTimeout is
//     dropped.
//
// Detach readiness: a detached engine (`backpack _engine ...`) is ready
// once `status` succeeds and Snapshot.Lease.Held is true. The detaching
// parent polls status every DetachPollInterval for up to
// DetachReadyTimeout, and fails early if the child exits. A startup
// failure is logged to the engine log and exits non-zero.

// EngineOp names an engine operation.
type EngineOp string

const (
	OpEngineStatus     EngineOp = "status"
	OpEngineStop       EngineOp = "stop"
	OpEngineExtend     EngineOp = "extend"
	OpEngineAbortSleep EngineOp = "abort_sleep"
	OpEngineSubscribe  EngineOp = "subscribe"
)

// Engine limits and timings.
const (
	MaxEngineRequestBytes  = 4 * 1024
	MaxEngineResponseBytes = 1024 * 1024
	// MaxExtendMinutes bounds extend's minutes (one week).
	MaxExtendMinutes      = 7 * 24 * 60
	SubscribeWriteTimeout = 5 * time.Second
	// DetachReadyTimeout covers setup plus a first poll, which can take
	// ~14s when the network probe is slow or offline.
	DetachReadyTimeout = 30 * time.Second
	DetachPollInterval = 100 * time.Millisecond
)

// EngineRequest is one request line.
//
//	{"op":"status"}
//	{"op":"stop"}  {"op":"stop","sleep":true}  {"op":"stop","sleep":false}
//	{"op":"extend","minutes":30}  {"op":"extend","forever":true}
//	{"op":"abort_sleep"}
//	{"op":"subscribe"}
type EngineRequest struct {
	Op      EngineOp `json:"op"`
	Sleep   *bool    `json:"sleep,omitempty"`
	Minutes int      `json:"minutes,omitempty"`
	Forever bool     `json:"forever,omitempty"`
}

// Validate checks the request for its op. It returns a *RemoteError with
// CodeBadRequest on failure.
func (r EngineRequest) Validate() error {
	bad := func(format string, args ...any) error {
		return &RemoteError{Code: CodeBadRequest, Message: fmt.Sprintf(format, args...)}
	}
	switch r.Op {
	case OpEngineStatus, OpEngineSubscribe, OpEngineAbortSleep:
		if r.Sleep != nil || r.Minutes != 0 || r.Forever {
			return bad("%s takes no arguments", r.Op)
		}
	case OpEngineStop:
		if r.Minutes != 0 || r.Forever {
			return bad("stop takes only sleep")
		}
	case OpEngineExtend:
		if r.Sleep != nil {
			return bad("extend takes no sleep")
		}
		if r.Forever == (r.Minutes != 0) {
			return bad("extend needs exactly one of minutes or forever")
		}
		if !r.Forever && (r.Minutes < 1 || r.Minutes > MaxExtendMinutes) {
			return bad("minutes must be 1..%d", MaxExtendMinutes)
		}
	case "":
		return bad("missing op")
	default:
		return bad("unknown op %q", r.Op)
	}
	return nil
}

// EngineResponse is one response line (and each line of a subscribe
// stream).
type EngineResponse struct {
	Result
	Snapshot *contracts.Snapshot `json:"snapshot,omitempty"`
	// Done marks the last line of a subscribe stream.
	Done bool `json:"done,omitempty"`
}

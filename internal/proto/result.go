package proto

import (
	"errors"
	"fmt"

	"github.com/ilias-t/bckp/internal/contracts"
)

// ErrorCode is a machine-readable error class carried next to the human
// "error" string in every response; clients branch on it instead of
// parsing messages.
type ErrorCode string

const (
	// CodeBadRequest: malformed JSON, unknown field (daemon), unknown op,
	// or a missing/invalid argument.
	CodeBadRequest ErrorCode = "bad_request"
	// CodeTooLarge: the request line exceeded the size limit.
	CodeTooLarge ErrorCode = "too_large"
	// CodeNotAllowed: the peer uid is not in allowed_uids.
	CodeNotAllowed ErrorCode = "not_allowed"
	// CodeBusy: another lease is active, or the connection limit is hit.
	CodeBusy ErrorCode = "busy"
	// CodeNoLease: the op needs an active lease and there is none (never
	// acquired, released, expired, or dropped by a backstop).
	CodeNoLease ErrorCode = "no_lease"
	// CodeLeaseMismatch: a lease is active but the id is wrong or the
	// request arrived on a connection other than the one that acquired it.
	CodeLeaseMismatch ErrorCode = "lease_mismatch"
	// CodeNotAbortable (engine): a critical-triggered sleep is pending.
	CodeNotAbortable ErrorCode = "not_abortable"
	// CodeNoSleepPending (engine): abort_sleep with no sleep pending.
	CodeNoSleepPending ErrorCode = "no_sleep_pending"
	// CodeInternal: the server failed to carry out a valid request (e.g.
	// pmset failed, or SleepDisabled did not verify).
	CodeInternal ErrorCode = "internal"
)

// Result is the common response envelope. Every response type embeds it,
// so "ok", "error" and "code" are top-level keys on the wire.
type Result struct {
	OK    bool      `json:"ok"`
	Error string    `json:"error,omitempty"`
	Code  ErrorCode `json:"code,omitempty"`
}

// OKResult is a successful Result.
func OKResult() Result { return Result{OK: true} }

// Fail builds a failed Result.
func Fail(code ErrorCode, format string, args ...any) Result {
	return Result{OK: false, Code: code, Error: fmt.Sprintf(format, args...)}
}

// Err converts a failed Result into a *RemoteError, or returns nil when OK.
func (r Result) Err() error {
	if r.OK {
		return nil
	}
	return &RemoteError{Code: r.Code, Message: r.Error}
}

// RemoteError is a failure reported by the peer. errors.Is maps codes onto
// the contracts sentinels:
//
//	busy                     -> contracts.ErrLeaseBusy
//	no_lease, lease_mismatch -> contracts.ErrLeaseLost
//	not_allowed              -> contracts.ErrNotAllowed
//	not_abortable            -> contracts.ErrNotAbortable
//	no_sleep_pending         -> contracts.ErrNoSleepPending
type RemoteError struct {
	Code    ErrorCode
	Message string
}

func (e *RemoteError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("remote error (%s)", e.Code)
	}
	return fmt.Sprintf("remote error (%s): %s", e.Code, e.Message)
}

// Is implements errors.Is against the contracts sentinels.
func (e *RemoteError) Is(target error) bool {
	switch e.Code {
	case CodeBusy:
		return target == contracts.ErrLeaseBusy
	case CodeNoLease, CodeLeaseMismatch:
		return target == contracts.ErrLeaseLost
	case CodeNotAllowed:
		return target == contracts.ErrNotAllowed
	case CodeNotAbortable:
		return target == contracts.ErrNotAbortable
	case CodeNoSleepPending:
		return target == contracts.ErrNoSleepPending
	}
	return false
}

// CodeOf returns the ErrorCode of a *RemoteError in err's chain, or "".
func CodeOf(err error) ErrorCode {
	var re *RemoteError
	if errors.As(err, &re) {
		return re.Code
	}
	return ""
}

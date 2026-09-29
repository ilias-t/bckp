package engine

import (
	"fmt"

	"github.com/ilias-t/bckp/internal/contracts"
)

// ActionKind says what the engine loop should do for an Action returned by
// one of the pure state-machine Step functions.
type ActionKind int

const (
	// ActNotify: send Action.Notification.
	ActNotify ActionKind = iota
	// ActLog: log Action.Msg (no user-visible effect).
	ActLog
	// ActSleepPending: a sleep grace period started; see the new state's
	// Pending.
	ActSleepPending
	// ActSleepCancel: a pending sleep was cancelled.
	ActSleepCancel
	// ActSleepNow: the grace period is over; execute the sleep.
	ActSleepNow
	// ActEndSession: end the session with Action.EndReason (no sleep).
	ActEndSession
	// ActRecover: toggle the hotspot network service off and on; report
	// back with RecoveryDone.
	ActRecover
	// ActPreferHotspot: the hotspot connected while the preference is
	// auto; move it ahead of Wi-Fi now.
	ActPreferHotspot
	// ActWiFiNudge: cycle Wi-Fi power (or turn it on when off) so macOS
	// auto-joins the iPhone hotspot; report back with NudgeDone.
	ActWiFiNudge
)

var actionKindNames = map[ActionKind]string{
	ActNotify: "notify", ActLog: "log", ActSleepPending: "sleep-pending",
	ActSleepCancel: "sleep-cancel", ActSleepNow: "sleep-now", ActEndSession: "end-session",
	ActRecover: "recover", ActPreferHotspot: "prefer-hotspot", ActWiFiNudge: "wifi-nudge",
}

func (k ActionKind) String() string {
	if n, ok := actionKindNames[k]; ok {
		return n
	}
	return fmt.Sprintf("ActionKind(%d)", int(k))
}

// Action is one side effect requested by a state machine. Only the fields
// relevant to Kind are set.
type Action struct {
	Kind         ActionKind
	Notification contracts.Notification
	// Msg is the log line (ActLog) or a human reason (other kinds).
	Msg       string
	EndReason contracts.EndReason
	Trigger   contracts.SleepTrigger
	// NextPoll holds an ActNotify until the next poll (or the session's
	// end, if sooner): "back online" is sent over a link seconds old,
	// where the first request often times out.
	NextPoll bool
}

// notifyAttention and notifyInfo build ActNotify actions. Every call site
// picks the class (contracts.NotifyClass): attention is always pushed;
// info is only logged unless notify.push = "all".
func notifyAttention(priority int, title, body string, tags ...string) Action {
	return notifyClass(contracts.ClassAttention, priority, title, body, tags...)
}

func notifyInfo(priority int, title, body string, tags ...string) Action {
	return notifyClass(contracts.ClassInfo, priority, title, body, tags...)
}

func notifyClass(class contracts.NotifyClass, priority int, title, body string, tags ...string) Action {
	return Action{Kind: ActNotify, Notification: contracts.Notification{
		Priority: priority, Title: title, Body: body, Tags: tags, Class: class,
	}}
}

func logf(format string, args ...any) Action {
	return Action{Kind: ActLog, Msg: fmt.Sprintf(format, args...)}
}

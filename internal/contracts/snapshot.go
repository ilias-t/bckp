package contracts

import "time"

// FSMState is the engine's thermal state-machine state (docs/DESIGN.md
// "State machines"). The values are upper-case and stable; the TUI renders them.
type FSMState string

const (
	FSMNominal      FSMState = "NOMINAL"
	FSMFair         FSMState = "FAIR"
	FSMHot          FSMState = "HOT"
	FSMSleepPending FSMState = "SLEEP_PENDING"
	FSMSleeping     FSMState = "SLEEPING"
)

// SleepTrigger says what started a pending sleep.
type SleepTrigger string

const (
	// SleepTriggerSerious: HOT for serious_sleep_after. Abortable.
	SleepTriggerSerious SleepTrigger = "serious"
	// SleepTriggerCritical: critical for critical_polls. Never abortable.
	SleepTriggerCritical SleepTrigger = "critical"
	// SleepTriggerCPUTemp: optional cpu_sleep_c threshold. Abortable.
	SleepTriggerCPUTemp SleepTrigger = "cpu_temp"
)

// EndReason says why a session ended.
type EndReason string

const (
	EndStopped      EndReason = "stopped"       // backpack stop, the TUI's Ctrl-C, or SIGINT
	EndSignal       EndReason = "signal"        // SIGTERM or SIGHUP: logout, kill, a closed terminal
	EndExpired      EndReason = "expired"       // requested duration elapsed
	EndMaxDuration  EndReason = "max_duration"  // max_duration cap reached
	EndBatteryFloor EndReason = "battery_floor" // battery at or below the floor
	EndThermalSleep EndReason = "thermal_sleep" // thermal sleep executed
	EndUserSleep    EndReason = "user_sleep"    // stop --sleep executed
	EndLeaseLost    EndReason = "lease_lost"    // daemon lease lost and not re-acquired
	EndError        EndReason = "error"         // fatal engine error; see Snapshot.Errors
)

// NetworkMode is the engine's network feature level for the session.
type NetworkMode string

const (
	NetworkOff           NetworkMode = "off"            // no watchdog and no hotspot preference
	NetworkWatchdog      NetworkMode = "watchdog"       // outage detection + recovery only
	NetworkPreferHotspot NetworkMode = "prefer_hotspot" // hotspot moved ahead of Wi-Fi (+ watchdog when on)
	// NetworkHotspotAuto: probing, waiting for the hotspot to connect
	// (network.prefer_hotspot = "auto"). Once it does, the engine moves it
	// ahead of Wi-Fi and the mode becomes NetworkPreferHotspot for the rest
	// of the session.
	NetworkHotspotAuto NetworkMode = "hotspot_auto"
)

// Session option names (Snapshot.Options), in the order the CLI asks them.
const (
	OptionDuration = "duration"
	OptionHotspot  = "hotspot"
	OptionWatchdog = "watchdog"
)

// Session option sources (SessionOption.Source).
const (
	// SourceFlag: given on the command line (a flag or the DUR argument).
	SourceFlag = "flag"
	// SourcePrompt: answered at a TTY prompt.
	SourcePrompt = "prompt"
	// SourceDefault: not asked; the config default (or built-in default).
	SourceDefault = "default"
)

// SessionOption is one session option's effective value and where it came
// from, as resolved by the CLI when the session started. The engine
// reports them unchanged so `status` can show them.
type SessionOption struct {
	// Name is OptionDuration, OptionHotspot or OptionWatchdog.
	Name string `json:"name"`
	// Value is canonical: "forever" or a duration ("90m", "2h") for
	// duration; "auto"/"on"/"off" for hotspot; "on"/"off" for watchdog.
	Value string `json:"value"`
	// Source is SourceFlag, SourcePrompt or SourceDefault.
	Source string `json:"source"`
}

// NotificationStatus is the delivery state of a recorded notification.
type NotificationStatus string

const (
	NotificationSent   NotificationStatus = "sent"
	NotificationQueued NotificationStatus = "queued" // held in the outbox
	NotificationFailed NotificationStatus = "failed" // dropped after retries
	// NotificationLogged: an info notification that was only logged, not
	// pushed (notify.push = "attention").
	NotificationLogged NotificationStatus = "logged"
)

// MaxSnapshotNotifications caps Snapshot.Notifications.
const MaxSnapshotNotifications = 10

// Snapshot is the engine's full state, returned by `status` and streamed
// by `subscribe`. All fields describe the moment At.
type Snapshot struct {
	At time.Time `json:"at"`
	// PID is the engine process id.
	PID int `json:"pid"`

	Session SessionStatus `json:"session"`
	// Options are the session options as chosen at start, in prompt
	// order. Empty for engines started without them (tests, simulate).
	Options []SessionOption `json:"options,omitempty"`
	Thermal ThermalStatus   `json:"thermal"`
	// Power is the latest power reading, nil before the first poll.
	Power   *PowerReading `json:"power"`
	Network NetworkStatus `json:"network"`
	Lease   LeaseStatus   `json:"lease"`

	// SleepPending is set while the thermal FSM is SLEEP_PENDING.
	SleepPending *SleepPending `json:"sleep_pending,omitempty"`

	// Notifications holds the most recent notifications, oldest first,
	// at most MaxSnapshotNotifications.
	Notifications []NotificationRecord `json:"notifications,omitempty"`
	// OutboxPending counts notifications waiting in the disk outbox.
	OutboxPending int `json:"outbox_pending"`

	// Errors holds the latest error per component, keyed by "thermal",
	// "power", "network", "lease", "notify" or "engine". A key is removed
	// once that component succeeds again.
	Errors map[string]string `json:"errors,omitempty"`
}

// SessionStatus is the session timer.
//
// The effective end is the earlier of EndsAt (unless Forever) and CapAt.
// `extend` moves EndsAt (or sets Forever) and resets CapAt to
// now + max_duration.
type SessionStatus struct {
	StartedAt time.Time `json:"started_at"`
	// EndsAt is the user-requested end; zero when Forever.
	EndsAt time.Time `json:"ends_at,omitzero"`
	// Forever is true for `backpack forever` and a bare `backpack`.
	Forever bool `json:"forever"`
	// CapAt is the max_duration deadline.
	CapAt time.Time `json:"cap_at"`
	// RemainingS is the whole seconds from Snapshot.At to the effective
	// end, floored at 0.
	RemainingS int64 `json:"remaining_s"`
	// CapWarned is true once the "ending in 10 minutes" warning was sent
	// for the current CapAt.
	CapWarned bool `json:"cap_warned,omitempty"`

	// Ended is true in the final snapshot of a session.
	Ended     bool      `json:"ended,omitempty"`
	EndReason EndReason `json:"end_reason,omitempty"`
}

// ThermalStatus is the thermal state machine plus the latest reading.
type ThermalStatus struct {
	FSM FSMState `json:"fsm"`
	// Reading is the latest reading, nil before the first poll.
	Reading *ThermalReading `json:"reading"`
	// HotSince is when the continuous HOT period started (zero if not HOT
	// or SLEEP_PENDING).
	HotSince time.Time `json:"hot_since,omitzero"`
}

// NetworkStatus is the network watchdog state.
type NetworkStatus struct {
	Mode   NetworkMode `json:"mode"`
	Online bool        `json:"online"`
	// OfflineSince is when the current outage began (zero when online).
	OfflineSince time.Time `json:"offline_since,omitzero"`
	// LastRecovery is when the last service off/on toggle ran.
	LastRecovery time.Time `json:"last_recovery,omitzero"`
	// Recoveries counts toggles this session.
	Recoveries int `json:"recoveries"`
	// Link is how the latest probe reaches the network ("" before the
	// first probe or when Mode is NetworkOff).
	Link NetLink `json:"link,omitempty"`
	// WiFiNudges counts Wi-Fi nudges this session: Wi-Fi power cycled
	// (or turned on) while offline so macOS auto-joins the iPhone hotspot
	// (network.prefer_hotspot = "on" only).
	WiFiNudges int `json:"wifi_nudges"`
	// LastWiFiNudge is when the last nudge started.
	LastWiFiNudge time.Time `json:"last_wifi_nudge,omitzero"`
	// LastWiFiNudgeResult is the last nudge's outcome: "landed_hotspot",
	// "other_network", "still_offline" or "failed" ("" while pending).
	LastWiFiNudgeResult string `json:"last_wifi_nudge_result,omitempty"`
	// Latest is the latest probe, nil before the first poll or when Mode
	// is NetworkOff.
	Latest *NetSnapshot `json:"latest"`
}

// LeaseStatus is the engine's view of its daemon lease.
type LeaseStatus struct {
	Held    bool      `json:"held"`
	ID      string    `json:"id,omitempty"`
	Expires time.Time `json:"expires,omitzero"`
}

// SleepPending is a scheduled sleep in its grace period.
type SleepPending struct {
	Trigger SleepTrigger `json:"trigger"`
	Reason  string       `json:"reason"`
	// At is when the grace period ends and the sleep executes.
	At time.Time `json:"at"`
	// RemainingS is whole seconds from Snapshot.At to At, floored at 0.
	RemainingS int64 `json:"remaining_s"`
	// Abortable is false for SleepTriggerCritical.
	Abortable bool `json:"abortable"`
}

// NotificationRecord is a notification the engine emitted.
type NotificationRecord struct {
	Notification Notification       `json:"notification"`
	At           time.Time          `json:"at"`
	Status       NotificationStatus `json:"status"`
	Err          string             `json:"err,omitempty"`
}

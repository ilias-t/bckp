# backpack design

backpack keeps a lid-closed MacBook awake for a bounded session, watches its
heat and battery, puts it to sleep before it cooks, and keeps an iPhone
hotspot link healthy. This document covers how it is built and why.

## Background

These are the macOS facts the design rests on. They were verified on Apple
Silicon with macOS 27.

- **Only `pmset -a disablesleep 1` stops lid-close sleep without an external
  display.** `caffeinate`, IOPMAssertions and apps built on them (such as
  Amphetamine) prevent idle sleep, but closing the lid still sleeps the Mac
  (`'Clamshell Sleep'` in `pmset -g log`). With `SleepDisabled=Yes`, a
  closed lid with no display stayed awake with no sleep events at all.
- **That setting is dangerous to leave on.**
  - It needs root.
  - It is one system-wide flag.
  - It persists across reboots.
  - It also blocks the kernel's own low-battery and thermal emergency sleep.

  So it must never outlive the session that set it.
- **Readings need no root.**
  - `ioreg -r -k AppleClamshellState -d 4` shows `SleepDisabled`,
    `AppleClamshellState` and `AppleClamshellCausesSleep`.
  - `NSProcessInfo.thermalState` gives nominal, fair, serious or critical.
  - `pmset -g batt` gives the power source and charge.
  - `macmon`, when installed, gives CPU and GPU °C. The idle GPU sensor reads
    about 4°C, so readings under 15°C are dropped.
  - The battery's fuel gauge publishes "gas gauge battery" temperature
    services through the private `IOHIDEventSystemClient` API (usage page
    0xff00, usage 5). The cgo shim reads them in-process and keeps the
    hottest reading within 1–99°C; other sensors on that page read
    nonsense such as -9202°C.
- **The thermal state misses heat in a bag.** It tracks throttling
  pressure, not temperature. A closed MacBook in a bag can heat its case
  and battery while the chip stays cool enough that the state reads
  nominal throughout ([Lidless #34](https://github.com/nghialuong/Lidless/issues/34)
  saw 75 minutes of this). The battery is the part at risk: Apple says
  ambient heat above 35°C can permanently damage battery capacity, and
  the chip throttles and shuts itself down before it is harmed. So
  battery °C is a safety input, not advisory.
- **Network changes need no root for admin users.**
  `networksetup -ordernetworkservices`, `-setnetworkserviceenabled` and
  `-setairportpower` all work. `networksetup` exits 0 even when it fails, so
  its output is parsed.
- **The iPhone USB link can drop and stay down.** There is no default route
  and macOS does not recover it on its own. Turning the service off and on
  again recovers it without root.
- **Command-line tools can't see Wi-Fi network names.** Without Location
  Services permission, macOS redacts every SSID (`system_profiler
  SPAirPortDataType` shows `<redacted>`). `networksetup -setairportnetwork en0
  <name>` then fails with "Could not find network", and still exits 0.
- **Instant Hotspot can be triggered by cycling Wi-Fi.** With System Settings
  → Wi-Fi → Ask to join hotspots → Automatic, turning Wi-Fi off and on makes
  macOS look for the iPhone's hotspot. It wakes the hotspot over Bluetooth,
  even with the phone locked in a pocket. In testing it joined after about
  19 seconds.
- **Some settings can't be read.** The "Ask to join hotspots" setting is in
  `/Library/Preferences/SystemConfiguration/com.apple.airport.preferences.plist`.
  That file is mode 0644, but reads fail with EPERM outside Apple's own
  processes, so backpack can't check the setting.

## Architecture

```
backpack CLI ──unix socket──> engine (user, one per user) ──unix socket──> backpackd (root)
TUI ──subscribe──┘            providers: thermal, macmon, power,           runs: pmset, ioreg
                              network, notifiers
```

### Processes

- **`backpack`** (alias `bckp`, a symlink) is the CLI, the TUI and the
  engine.
  - In a terminal, starting a session runs the engine as a detached child
    and attaches the TUI. `q` detaches and Ctrl-C stops the session (after a
    confirm).
  - With no terminal (ssh from a phone, an agent's shell), it detaches, waits
    until the engine holds its lease, prints one line and exits 0. The
    caller never hangs.
- **The engine** runs as the user.
  - It owns the session timer, the state machines and the notification
    policy.
  - It serves newline-delimited JSON on `~/Library/Application
    Support/backpack/engine.sock`: `status`, `stop`, `extend`, `abort_sleep`
    and `subscribe`.
  - A `flock` on `engine.lock` keeps it to one engine per user.
  - Go cannot fork, so detaching re-execs `backpack _engine ...` with
    `Setsid`.
- **`backpackd`** is a root LaunchDaemon. It is the only process that
  changes `disablesleep`.

### Code layout

| Package | Role |
|---|---|
| `cmd/backpack`, `cmd/backpackd` | Wiring: real providers, notifiers and daemon client. |
| `internal/cli` | Commands, prompts, install, doctor and logs. It imports no engine, TUI or notifier code, and no provider code except pure parsers (`power.ParseClamshell`); everything else comes through `cli.Wiring`. |
| `internal/engine` | The session loop and pure state machines. `engine/ipc` serves the engine protocol. |
| `internal/daemon` | backpackd. `daemon/client` implements `contracts.SleepGuard`. |
| `internal/providers/*` | Thermal (a cgo shim for `thermalState`, plus macmon), power, network. |
| `internal/notify/*` | ntfy, macOS banners (osascript), the log (for `notify test`), and a disk-backed outbox. |
| `internal/contracts` | Interfaces and data types shared by all of the above. |
| `internal/fakes` | Fakes for every interface, a fake clock and a fake `Exec`. |
| `internal/tui` | The bubbletea dashboard. |
| `internal/eventlog` | Rotating JSONL event log, and the `pmset -g log` parser. |
| `internal/humanize` | The one formatter for durations, clock times and on/off in everything people read. |

Every external command runs through `contracts.Exec` with an absolute path
from `internal/paths`. Nothing looks up `PATH`, and tests replace `Exec` with
recorded output.

## Safety model

### The root helper

`backpackd` is installed once with `sudo backpack install`.

- **Files:**
  - `/Library/PrivilegedHelperTools/backpackd`, root:wheel 0755.
  - `/Library/LaunchDaemons/dev.bckp.helper.plist`.
  - `/Library/Application Support/backpack/allowed_uids`.
- **Commands:** it runs only three fixed argv arrays, `pmset -a disablesleep
  1`, `pmset -a disablesleep 0` and `pmset sleepnow`, plus a read-only
  `ioreg`. There is no shell, and nothing from a client reaches an argv.
- **Operations:** `acquire`, `renew`, `release`, `sleep` and `status`.
- **Who can connect:** the socket `/var/run/backpackd.sock` is mode 0666, but
  every peer's uid is checked with `LOCAL_PEERCRED` against the root-owned
  allowlist written at install. The allowlist, not the file mode, gates
  access.
- **Input limits:**
  - TTL is clamped to 30–300s.
  - `reason` is capped at 128 characters and only logged.
  - Requests are capped at 4 KB.
  - At most 8 connections.
- **Leases:** one lease at a time, bound to the connection that acquired it.
- **Verification:** every flag change is checked against the kernel's
  `SleepDisabled` (ioreg) and retried once.
- **Root never runs a user-writable binary.** macmon lives in Homebrew's
  user-writable prefix, so the daemon's thermal source uses only the
  in-process shim, and the macmon reader refuses to run as root.

### Dead-man's switch

`disablesleep 1` is only held through a lease, with independent fallbacks:

1. **Normal end, `stop`, Ctrl-C, SIGTERM, SIGHUP:** the engine restores the
   network and releases the lease.
2. **Engine crash or `kill -9`:** the lease is bound to the engine's
   connection. When it closes, the daemon runs `disablesleep 0` at once. In
   testing this took about 79 ms.
3. **Engine hung but connected:** the lease has a 90s TTL. Only the
   engine's own loop renews it (every 30s, `proto.RenewInterval`), so a
   wedged loop stops renewing and the lease expires.
4. **Daemon crash:** launchd's `KeepAlive` restarts it. The daemon resets the
   flag on start, before listening, and on SIGTERM.
5. **Reboot:** `RunAtLoad` resets the flag, and sessions never resume on
   their own.
6. **Hard-coded backstops in the daemon:** it polls every 10s. Two critical
   thermal readings in a row, two battery readings at or above 50°C, or the
   internal battery under 10%, end the lease and put the Mac to sleep.
   These are not configurable; `thermal.battery_*_c` cannot exceed 50.
7. **Idle check:** with no lease, the daemon reads `SleepDisabled` every 60s
   and resets it when it is `Yes`. A reset that did not verify is retried
   at the next check. A manual `sudo pmset -a disablesleep 1` is undone
   within about a minute while the helper is installed (`flag.reconcile`).
8. **Tools:** `backpack doctor` flags `SleepDisabled=Yes` with no lease, and
   `uninstall` resets the flag first.

A gated end-to-end test (`BACKPACK_E2E=1`) acquires a real lease, `kill -9`s
the holder, and asserts that `SleepDisabled` reads `No` within 2 seconds.

## State machines

The engine polls every 15s. Each state machine is a pure `Step(state,
readings, now) -> (state, actions)` function, table-tested with a fake
clock.

### Thermal

| State | Entered when | Action |
|---|---|---|
| NOMINAL | — | — |
| FAIR | 2 fair polls in a row | Info notification, at most once per 30m. |
| HOT | Serious, entered at once | Alert, repeated every 10m. The hot timer starts. |
| SLEEP_PENDING | HOT for 3 minutes, critical for 2 polls, or battery °C at `battery_sleep_c` (45) or `battery_critical_c` (50) for 2 polls | Alert: the Mac sleeps in 30s (15s when critical or at `battery_critical_c`). A serious or `battery_sleep_c` sleep can be aborted (`abort`, `extend`, `stop --no-sleep`); after an abort that trigger stays quiet for 3 minutes. A critical one cannot. |
| SLEEPING | Grace period over | Restore the network, flush notifications (at most 5s), ask the daemon to sleep, then exit. |

- Eight polls (2 minutes) below serious reset the hot timer and send
  "cooled down", so a Mac swinging between serious and fair still sleeps.
  "Back to nominal" needs 3 polls.
- Battery °C at `battery_warn_c` (40) for 2 polls sends one warning,
  re-armed after 8 polls below it. Four polls without a battery reading
  send "Battery temperature unavailable" once.
- CPU °C is advisory. `thermal.cpu_sleep_c` adds an optional CPU °C
  trigger, off by default: the chip runs near 100°C under normal load and
  stays cool in a bag, so it says little about bag heat.

### Power

- **AC → battery:** alert, and keep running.
- **Battery floor** (default 25%): alert, then end the session. Releasing
  the lease lets a closed lid sleep normally.
- **`max_duration`** (default 12h): caps every session, including `forever`.
  It warns 10 minutes before the end, and `extend` resets it.

### Network

- **Watchdog.**
  - Offline means no default route, or the HTTP probe failed. The probe
    tries ntfy's health endpoint and then `captive.apple.com`, so an ntfy
    outage alone is not an outage.
  - After 90s offline with the iPhone interface present, the watchdog turns
    the iPhone USB service off, waits 3s and turns it on again. It does this
    at most once every 180s.
  - When the link returns, "back online" reports the outage window, the
    diagnostics and the attempts.
- **Diagnostics while offline.** Three checks run concurrently:
  - DNS through the system resolver.
  - DNS through 1.1.1.1.
  - A ping to 1.1.1.1.

  This tells a broken VPN resolver apart from a dead link. A VPN can
  intercept port 53 to any server, though, so "direct" DNS is not always
  direct.
- **Hotspot preference.**
  - `auto` (the default): moves "iPhone USB" ahead of Wi-Fi the first time
    the phone is connected. Connected means its interface is up and its
    gateway answers. A plugged-in phone that is not sharing would otherwise
    take the default route onto a dead link.
  - `on`: moves it at start.
  - `off`: never moves it.

  A restore journal (`restore.json`) is written before any change. At the
  end, the original order and Wi-Fi power are always restored. A journal
  left by a crash is restored by the next `start`, `stop` or `doctor --fix`.
- **Wi-Fi nudge** (`prefer_hotspot = "on"` only).
  - Trigger: the Mac has been offline for `wifi_nudge_after` (60s) and the
    phone is not on USB.
  - Action: backpack cycles Wi-Fi so macOS's "Ask to join hotspots" can find
    the iPhone.
  - Limits: at most once per `wifi_nudge_every` (3m), and never while online.
    A nudge while online would drop a working connection, and macOS would
    rejoin the same network.
  - Recognising the hotspot: over Wi-Fi it is recognised by its fixed
    gateway, 172.20.10.1.
  - Result: the first online probe after Wi-Fi returns records where the Mac
    landed: `landed_hotspot`, `other_network` or `still_offline` after 45s.
  - Journal: the off phase is recorded in the journal, so a crash mid-nudge
    still restores Wi-Fi.
- **Escalation.** Three failed recoveries or nudges in one outage raise one
  "still offline" alert.

## Notification policy

Only what needs attention is pushed. Everything is logged.

- **Pushed (attention):**
  - Heat: serious, critical, a pending or actual sleep, and "cooled down"
    after one of those.
  - Power: AC → battery, and the battery floor.
  - Session end: any end on battery; the `max_duration` warning and the cap
    itself; any end the user did not ask for. A SIGTERM or SIGHUP (a
    logout, `kill`, a closed terminal) ends the session as `signal`, which
    is pushed.
  - Failures: lease lost, failing sensor readings (and their recovery),
    hotspot preference dropped, network restore failed, and "still offline"
    after 3 attempts.
  - "Back online" after an outage of at least `notify.outage_min` (5m).
- **Logged only (info):**
  - Start, extend, the end of the requested time, and a stop the user
    asked for (`stop`, Ctrl-C/SIGINT, ending as `stopped`).
  - Fair/warming, back to nominal, back on AC.
  - Short outages and the heartbeat. The heartbeat is off by default.

Details of delivery:

- `notify.push = "all"` pushes info too.
- `backpack notify test` always pushes.
- **Backends:**
  - macOS notifications are on by default, so alerts reach the user with no
    setup.
  - ntfy is added when `notify.ntfy_topic` is set, for the phone. A
    generated topic is 24 random characters with nothing identifying: ntfy.sh
    topics are public to anyone who knows the name.
  - Every notification, pushed or not, is recorded in the event log
    (`notify.*`). The log backend only serves `notify test`.
- **Outbox:** failed ntfy sends wait in a disk-backed outbox. It is retried
  on the next successful send, and every 30s while the network is watched
  and online.
- **"Back online" is held one poll:** the first request over a link seconds
  old often times out.
- **Redaction:** the ntfy topic is never logged; strings that could contain
  it are redacted.

## Session options and prompts

Duration, hotspot preference and watchdog are declared once, in
`internal/cli/options.go`. That table drives flag parsing, the prompts, the
one-line summary and `status`, which shows each value and its source (flag,
prompt or default).

- **In a terminal:** every option not given as a flag is asked, with the
  default in brackets. `-y` takes the defaults. The hotspot question is
  skipped when the hotspot's network service (`network.service`) does not
  exist, i.e. no iPhone was ever connected over USB.
- **Without a terminal:** nothing is asked and stdin is never read.
- **Setup prompts (terminal only):**
  - Offer to install the helper when it is missing.
  - A short first-run setup when there is no config: an optional ntfy
    topic. The config it writes sets only what the user chose; the other
    keys stay commented out, so new defaults in later releases apply.
  - Without the helper, no session starts.
- **Not session options:** safety rails and thresholds live only in the
  config file.

## Key decisions

- **A root LaunchDaemon instead of sudoers rules.** sudoers can grant
  `pmset`, but nothing would undo `disablesleep 1` after a `kill -9` or a
  reboot. A root watchdog is needed either way, so it is the only thing
  holding root, and it does one job. The flip side: while the helper is
  installed, backpack owns the flag, and its idle check undoes a manual
  `pmset -a disablesleep 1` within a minute.
- **Leases bound to a connection.** A crash closes the socket, which the
  kernel guarantees. The daemon reacts to that at once instead of waiting
  for a TTL.
- **Bounded sessions.** A bare `backpack` runs until stopped, capped at 12h.
  "Forever" is the common case, but a forgotten session must still end.
- **Keep running on battery, stop at a floor.** Unplugging from a battery
  pack is normal in a bag. Running the Mac flat with sleep disabled is not.
- **Serious heat is abortable, critical is not.** Serious heat is often a
  short burst from a build; critical heat is not negotiable.
- **Nudge instead of join by name.** SSIDs are redacted for command-line
  tools, so joining a network by name cannot work. backpack never reads,
  stores or passes an SSID.
- **Known networks win.** macOS prefers networks it already knows. A nudge
  may land on one (recorded as `other_network`), and backpack won't nudge
  while it works. This is documented rather than worked around.
- **Attention-only pushes.** A phone that buzzes for every start and stop
  gets muted, and then the heat alert is missed.
- **Go, bubbletea, cgo for one call.** Go and bubbletea give single binaries
  and a good TUI. The only cgo is the `thermalState` shim. Everything else
  shells out through a fakeable `Exec`.
- **No always-on agent.** backpack only runs during a session; there is
  nothing to keep running between sessions.

## Observability

`backpack logs --since 2h` merges these sources into one timeline:

- The engine's, the CLI's and the daemon's JSONL event logs.
- macOS's own Sleep, Wake and DarkWake lines from `pmset -g log`.

`--bundle` writes a tarball for sharing, with the config redacted. The event
taxonomy is in [`internal/eventlog/events.md`](../internal/eventlog/events.md).

## Testing

- **State machines:** table tests with a fake clock.
- **Providers:** tested against recorded `pmset`, `ioreg`, `networksetup`,
  `route`, `ifconfig`, `dig` and `macmon` output through `fakes.Exec`.
  Addresses and identifiers are anonymized.
- **Daemon:** tested with a fake pmset runner.
- **`backpack simulate`:** runs the real engine on scripted readings with a
  fake clock, and renders its timeline from the engine's event log. It
  never sends or sleeps.
- **Nothing touches the real system:** no test in `go test ./...` runs
  `pmset`, `networksetup` or `sudo`, or posts a real notification. The one
  exception is the opt-in `BACKPACK_E2E=1` test above.

# Event taxonomy

Every record is one JSON line with these fields:

- `ts` (RFC 3339, in milliseconds)
- `level` (`DEBUG`, `INFO`, `WARN` or `ERROR`)
- `msg`
- `src` (`engine`, `daemon`, `cli` or `macos`)
- `event`

`msg` and `event` hold the same name, except for free-text `*.note` records.

Other conventions:

- Durations are strings such as `"1m30s"`.
- Temperatures are `*_c` numbers.
- `battery` is a percentage.
- The ntfy topic is never logged. Error strings that could contain it are redacted to `<redacted>`.

## Files

| Source | File | Mode |
|---|---|---|
| engine | `~/Library/Logs/backpack/engine.jsonl` (+ `engine-YYYYMMDDTHHMMSS.jsonl`) | dir 0700, files 0600 |
| cli | `~/Library/Logs/backpack/cli.jsonl` (+ rotated files) | dir 0700, files 0600 |
| daemon | `/Library/Logs/backpack/backpackd.jsonl` (+ rotated files) | root:wheel, dir 0755, files 0644 |
| macos | `pmset -g log`, parsed on read, never stored | — |

**Rotation:**

- The active file rolls over at 2 MB.
- Rotated files older than `log.max_age` (7d) are deleted first.
- The oldest files are then deleted until the files, plus room for a full active file, fit within `log.max_mb` (50 MB).

The CLI has its own file rather than appending to `engine.jsonl`: a `Writer` tracks its file's size and rotates it in-process, so a second process writing the same file would have it renamed away underneath and would break the size cap. `bckp logs` (and `--follow` and `--bundle`) read every `*.jsonl` in the directory, so the files merge into one timeline. The engine's log honours `log.max_mb` and `log.max_age` (env `BACKPACK_LOG_MAX_MB`, `BACKPACK_LOG_MAX_AGE`); so does `cli.jsonl`. The daemon always uses the defaults.

**Crash safety:** a torn last line is terminated with a newline when the log is reopened, and readers skip it.

**Symlinks:** a log file or log directory that is a symlink, or a log path that is not a regular file, is refused rather than followed. The daemon then logs to stderr, which launchd writes to `/Library/Logs/backpack/backpackd.log`.

`engine.log` is not an event log. It holds the detached engine's stdout/stderr: panics and early startup errors.

## Engine (`src=engine`)

| Event | Level | Fields | When |
|---|---|---|---|
| `session.start` | info | `version`, `pid`, `options{duration,forever,hotspot,watchdog,network}`, `choices{duration,hotspot,watchdog}` (each `"value (source)"`, source `flag`/`prompt`/`default`; absent when started without the CLI), `ends`, `cap_at`, `config{poll,max_duration,battery_floor,cpu_sleep,serious_sleep_after,watchdog,prefer_hotspot,service,wifi_nudge_after,wifi_nudge_every,ntfy(bool),macos_notify,heartbeat,push,outage_min}` | Before the lease is requested. |
| `session.extend` | info | `minutes`, `forever`, `desc`, `cap_at` | `bckp extend` |
| `session.stop` | info/warn | `sleep` (`default`/`true`/`false`), `sleep_pending`, `signal` (`SIGTERM` or `SIGHUP`; absent for `stop` and Ctrl-C), `result` | `bckp stop`, Ctrl-C/SIGINT (end reason `stopped`), or SIGTERM/SIGHUP (end reason `signal`, pushed). Warn when refused because a critical sleep is pending. |
| `session.ending` | debug | `reason`, `detail` | The first end reason is recorded; cleanup follows. |
| `session.end` | info | `reason`, `detail`, `ran`, `slept`, `errors{...}` | Last record of a session, on every path. |
| `sample` | debug | `fsm`, `thermal`, `cpu_c`, `gpu_c`, `load`, `battery`, `ac`, `charging`, `lid`, `sleep_disabled`, `net` (default-route interface), `link` (`hotspot (USB)`, `hotspot (Wi-Fi)`, `Wi-Fi`, `other` or `none`), `online`, `wifi_nudges` (when any), `left` | First poll, then once a minute. Debug so it doesn't bury events; `bckp logs --level debug` shows it. |
| `thermal.transition` | info | `from`, `to` (FSM), `thermal`, `cpu_c`, `gpu_c`, `hot_for` | Any thermal FSM state change. |
| `sleep.pending` | warn | `trigger`, `reason`, `at`, `abortable`, readings | Grace period started. |
| `sleep.cancel` | info | `trigger`, `by` (`extend` or `abort`) | Pending sleep aborted by `extend` or by `abort` (which adds no time). A stop that cancels one is recorded as `session.stop` with `sleep_pending`. |
| `sleep.now` | warn | `trigger`, `reason` | Grace period over; the session ends in a sleep. |
| `sleep.request` / `sleep.failed` | warn / error | `reason` / `err`, `fallback` | The daemon's `sleep` op during shutdown. |
| `power.transition` | info | `from` (`unknown`/`AC`/`battery`), `to`, `battery`, `charging`, `lid`, `sleep_disabled` | First reading, then each AC↔battery switch. |
| `power.no_battery` | info | `err`, `note` | The Mac has no battery; the power rails are off. |
| `net.transition` | info | `from`, `to` (`online`/`offline`), `from_link`, `link` (as in `sample`), `iface`, `diag` | Online↔offline change, a first probe that is offline, or a change of link while online (e.g. `Wi-Fi` → `hotspot (Wi-Fi)`; then `from` and `to` are both `online`). Over Wi-Fi the hotspot is recognised by its gateway, 172.20.10.1. |
| `net.outage_start` | warn | `diag`, `hotspot_present`, `dns_sys`, `dns_direct`, `ip_ok` (`ok`/`fail`/`skip`), `verdict` | An outage begins. The DNS/IP checks run only while not online: DNS for the probe host through the system resolver and through 1.1.1.1, and a ping to 1.1.1.1. `verdict` names the likely cause, e.g. "system DNS failing, direct DNS works (VPN/Tailscale DNS?)". |
| `net.diag` | info | `offline_for`, `diag`, `dns_sys`, `dns_direct`, `ip_ok`, `verdict` (as in `net.outage_start`) | The diagnostic line changed during an outage. |
| `net.outage_end` | info | `duration`, `recoveries`, `first`, `diag` | Back online. |
| `net.recovery_start` | debug | `service`, `why` | Hotspot service disabled (phase 1). |
| `net.recovery_attempt` | info/warn/error | `service`, `why`, `result` (`ok`/`failed`/`refused`), `step` (`disable`/`re-enable`), `err`, `reenabled` (disable failed), `note`, `attempt` | Off/on toggle finished, or failed. Warn when disabling failed (the service is turned back on at once); error when re-enabling failed (retried every poll). `refused` (warn): the restore journal could not be written, so the service is not touched. |
| `net.recovery_retry` | debug/info | `service`, `err` / `result` | Re-enable retried each poll after a failure (debug); info with `result=ok` when a retry succeeds. |
| `net.order_change` | info | `changed`, `from`, `to` / `order`, `trigger` (`auto`), `why` | Hotspot moved ahead of Wi-Fi, or already ahead: at start (`on`), or with `trigger=auto` the first time the hotspot is connected (`auto`). |
| `net.wifi_change` | debug/error | `on`, `why` (`wifi nudge`), `result`, `err`, `note` | A Wi-Fi nudge turning Wi-Fi back on: debug when it did, error when it could not (retried every poll). |
| `net.wifi_nudge_start` | debug | `why`, `attempt`, `power_on_only` | A Wi-Fi nudge began (`prefer_hotspot = "on"` only): offline for `wifi_nudge_after` with the iPhone not on USB. Wi-Fi goes off for 3s and back on, or is just turned on when it was off. |
| `net.wifi_nudge` | info/warn | `reason`, `result` (`landed_hotspot`/`other_network`/`still_offline`/`failed`), `outcome` (the result in words), `attempt`, `power_on_only`, `offline_for`, `toggle` (off→on), `landed_after` + `link` (landed), `online_after` + `link` (other network), `waited` (still offline), `err` (failed) | A nudge's result: the first online probe after Wi-Fi came back (the hotspot over Wi-Fi, or another network), or still offline 45s after it (warn). `failed` (warn): Wi-Fi could not be turned off (it is turned back on right away). Never while online. |
| `net.wifi_unknown` | warn | `err` | Wi-Fi power can't be read; Wi-Fi is left alone. |
| `net.restore` | info/warn/error | `result`, `stale`, `path`, `order`/`wifi`/`service`/`mid_nudge`, `journal_kept`, `err` | Session network changes undone (info, only when something was changed), or a stale journal restored at start (warn, `stale`). Error when the restore failed: `err` joins every failed step (a pending toggle undo and the journal); `journal_kept` (the path) only when a journal exists. `mid_nudge`: the session ended between a nudge's off and on phases. |
| `net.setup_error` | error | `step`, `err`, `mode` | Hotspot preference dropped for the session (watchdog-only, or off when the watchdog is off). Also when an `auto` switch-over fails; it is not retried. |
| `net.journal_error` | error | `err`, `path` | The restore journal could not be written; the network change it was meant to cover is not made. |
| `lease.acquire` | info | `lease`, `ttl`, `expires` | The daemon granted the lease. |
| `lease.acquire_fail` | error | `err` | No lease; the session ends. |
| `lease.renew_fail` | warn | `lease`, `err`, `last_renew` | Transient renew error. |
| `lease.lost` | error | `lease`, `err`, `last_renew` | The daemon ended the lease; the session ends. |
| `lease.release` | info/error | `lease`, `result`, `err` | Released at the end of the session. |
| `notify.sent` | info | `title`, `priority`, `class` (`attention`/`info`), `pushed` (true) | The backend accepted the notification. |
| `notify.queued` | warn | `title`, `priority`, `class`, `pushed`, `err` | Saved to the outbox for retry. |
| `notify.failed` | error | `title`, `priority`, `class`, `pushed`, `err` | Send failed, or the send queue was full. |
| `notify.logged` | info | `title`, `priority`, `class` (`info`), `pushed` (false), `body` (redacted) | An info notification with `notify.push = "attention"` (the default): logged and listed in the TUI, not pushed. |
| `notify.held` | debug | `title`, `until` | "Back online" held until the next poll (or the session's end): the first request over a link seconds old often times out. |
| `notify.enqueue` | debug | `title`, `priority`, `class`, `pushed`, `body` (redacted) | Handed to the sender. |
| `notify.flush_timeout` | warn | `timeout`, `cut_off` | Notifications still pending at exit: `cut_off` of them are cut off. With ntfy set, the outbox keeps them for the next run. |
| `notify.outbox_flush` | info/debug | `result` (`ok`/`failed`), `delivered`, `pending`, `err` | The outbox was retried because a poll found the network online with messages waiting (at most every 30s). Debug when it failed. |
| `suspected_sleep` | warn | `gap`, `expected`, `last_poll`, `hint` | Poll gap longer than 3× the poll interval. Cross-check the `pm.*` events. |
| `provider.error` | warn | `kind` (`thermal`/`power`/`network`), `err`, `failures`, `suppressed` | At most once a minute per kind. |
| `provider.recovered` | info | `kind`, `failures`, `for` | The first good read after errors (summary). |
| `thermal.note` / `power.note` / `net.note` | debug | free-text `msg` | State-machine chatter. `net.note` includes, once per session with `prefer_hotspot = "on"`, "online via non-hotspot Wi-Fi: not nudging". |
| `engine.error` / `engine.panic` | error | `err` / `panic`, `stack`, `step` | Bad dependencies or config; a recovered panic. |

## CLI (`src=cli`, `cli.jsonl`)

| Event | Level | Fields | When |
|---|---|---|---|
| `session.detach` | info/error | `mode` (`detach`, `auto-detach` without a terminal, `tui`), `tty`, `yes`, `argv`, `ppid`, `ssh` (bool: `SSH_CONNECTION` set), `term` (`TERM_PROGRAM`), `choices{duration,hotspot,watchdog}` (`"value (source)"`), `pid` (engine), `result` (`ok`/`failed`) | The parent of a detached engine, once the engine holds its lease (or failed to). Says who started a session and how. |
| `notification` | info | `priority`, `title`, `body`, `tags` | `bckp notify test`'s log backend (no `event` field; readers use `msg`). Session notifications are not written here; the engine logs them as `notify.*`. |

## Daemon (`src=daemon`)

| Event | Level | Fields | When |
|---|---|---|---|
| `daemon.start` | info | `version`, `pid`, `socket`, `sleep_disabled` | Listening, after the reset on start. |
| `daemon.stop` | info | `reason` | Shutdown (signal, listen/accept failure). |
| `daemon.error` / `daemon.panic` | error | `err`, `step` / `panic` | Fatal errors; a panic resets the flag and re-panics. |
| `flag.set` | info/warn/error | `value`, `verified`, `attempt`, `err` | Every `pmset -a disablesleep` change, checked against ioreg. Warn on a failed first try; error when the retry fails too. |
| `flag.reconcile` | warn/error/info | `result` (`reset`/`failed`/`read_failed`/`read_ok`), `err`, `note`, `sleep_disabled` | The idle check: every 60s without a lease, `SleepDisabled` is read and reset when it is `Yes` (a reset that did not verify, or a manual `pmset -a disablesleep 1`). `reset` (warn): it was on and is now off. `failed` (error): the reset did not verify; retried at the next check. `read_failed` (warn) / `read_ok` (info): the ioreg read started failing / works again (logged on transitions only). |
| `lease.acquire` | info/error | `lease`, `uid`, `pid`, `ttl`, `reason` / `result`, `err` | A lease was granted, or `disablesleep 1` failed. |
| `lease.renew` | debug | `lease`, `expires` | Every renew (30s). |
| `lease.release` | info | `lease`, `why`, `held`, `sleep_disabled` | The client released the lease. |
| `lease.expire` | warn | `lease`, `why`, `held`, `sleep_disabled` | Not renewed within the TTL (engine hung). |
| `lease.conn_closed` | warn | `lease`, `why`, `held`, `sleep_disabled` | The lease connection closed (engine exited or crashed). |
| `lease.end` | warn | `lease`, `why` (`shutdown: …`, `sleep requested`, `backstop: …`), `held` | Any other end. |
| `backstop.trip` | error | `reason`, `lease` | Critical heat twice in a row, or battery below 10%. |
| `backstop.read_error` / `backstop.read_recovered` | warn / info | `source` | Backstop read failures, logged on transitions only. |
| `sleep.op` | info/error | `reason`, `result`, `err` | A client `sleep` op. |
| `sleep.now` | info/error | `result`, `err` | `pmset sleepnow` was run. |
| `auth.deny` | warn | `uid`, `pid`, `err` | The peer uid is not in the allowlist, or the allowlist is unsafe. Rate-limited (see `log.suppressed`). |
| `conn.reject` | warn | `reason`, `limit` | Connection limit reached. Rate-limited (see `log.suppressed`). |
| `log.suppressed` | warn | `suppressed_event`, `count`, `logged`, `over` | Any local process can trigger `auth.deny` and `conn.reject`, so each logs at most 10 records a minute. This one line then reports how many more were dropped: at the next such event after the minute, at the next idle check, or at shutdown. |

## macOS (`src=macos`, parsed from `pmset -g log` by `bckp logs`)

Only these type-column values are used. `Wake Requests` and `PM`/`Kernel Client Acks` also mention Sleep and Wake in their messages, so they are matched by column, not by message text.

| Event | Level | Fields | Source line |
|---|---|---|---|
| `pm.sleep` | info (**warn** for `'Clamshell Sleep'`) | `state` (`Sleep`/`DarkWake`), `reason`, `power`, `charge`, `secs`, `raw` | `Sleep` |
| `pm.wake` | info | `from`, `reason`, `power`, `charge`, `secs`, `raw` | `Wake` |
| `pm.darkwake` | info | `from`, `reason`, `power`, `charge`, `secs`, `raw` | `DarkWake` |
| `pm.thermal` | warn | `raw` | `ThermalEvent` (e.g. "Ignored DarkWake thermal emergency signal") |
| `pm.boot` | info | `raw` | `Start` (powerd started: boot) |

`secs` is how long that state lasted, taken from pmset's trailing "N secs".

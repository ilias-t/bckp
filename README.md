# backpack (`bckp`)

Keep a MacBook awake with the lid closed, in a bag, without cooking it.
backpack holds off lid-close sleep for a session. It watches heat and
battery, alerts your phone, and puts the Mac to sleep before it gets
dangerous.

The command is `bckp`; `backpack` is the same binary.

## Why

Some work should keep going while the Mac is in your bag: a remote coding
agent, a long build, a download, an ssh session. `caffeinate` and
Amphetamine prevent *idle* sleep, but closing the lid without an external
display still sleeps the Mac.

The only thing that stops that is `pmset -a disablesleep 1`, a root-only,
system-wide setting that survives reboots. It also disables the kernel's
own low-battery and overheat sleep. backpack holds that setting only for a
session, through a small root helper with a dead-man's switch. While the
session runs, it watches the heat and battery.

## Install

macOS 12 or later.

```sh
curl -fsSL https://raw.githubusercontent.com/ilias-t/bckp/main/install.sh | sh
```

The script:

- Downloads the latest release and checks its SHA-256.
- Runs `sudo backpack install` once to set up the root helper. sudo asks
  for your password.
- Puts `backpack`, `backpackd` and the `bckp` symlink in `~/.local/bin`
  (set `BACKPACK_BIN_DIR` to change it).
- Runs `backpack doctor`.

If `~/.local/bin` is not on your PATH, the script says so. Add it in
`~/.zshenv`, not `~/.zshrc`: `ssh mac bckp` runs a non-interactive shell,
which reads only `~/.zshenv`.

```sh
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zshenv
```

**Prefer to read it first?**

```sh
curl -fsSLO https://raw.githubusercontent.com/ilias-t/bckp/main/install.sh
less install.sh
sh install.sh
```

**Verify a release** (optional). Releases are built by GitHub Actions and
carry a build provenance attestation. With the
[gh CLI](https://cli.github.com), download the tarball from the release
page and run:

```sh
gh attestation verify bckp_<version>_darwin_universal.tar.gz --repo ilias-t/bckp
```

**From source** (needs Go and the Xcode Command Line Tools, for cgo):

```sh
go install github.com/ilias-t/bckp/cmd/...@latest
bin="$(go env GOBIN)"; bin="${bin:-$(go env GOPATH)/bin}"
sudo "$bin/backpack" install    # finds backpackd next to it
ln -s backpack "$bin/bckp"      # optional short name
```

## Use

```
$ bckp
How long? [forever] (12h cap) / 90 / 2h:
Prefer iPhone hotspot over Wi-Fi? [A]uto (when plugged in) / y / n:
Watch the network (outage alerts, hotspot link recovery)? [Y/n]:
bckp: forever (12h cap) · hotspot auto · watchdog on
```

Enter takes the default. The hotspot question only appears once an iPhone
has been connected over USB. A dashboard opens: `q` detaches (the session
keeps running), `s` stops it, `?` shows the keys.

| Command | What it does |
|---|---|
| `bckp 2h -y` | Start a 2-hour session, no questions. DUR is minutes (`90`), a duration (`1h30m`) or `forever`. |
| `bckp status` | What it's doing. Exits 3 when nothing runs; `--json` for scripts. |
| `bckp stop` | End the session; normal sleep rules are back. |
| `bckp extend 30` | 30 more minutes, or `extend forever`. Also restarts the 12h cap. |
| `bckp abort` | Cancel a pending heat sleep, without adding time. |
| `bckp attach` | Reopen the dashboard. |
| `bckp logs --since 2h` | What happened, including macOS's own sleep/wake log. |
| `bckp doctor` | Health checks. `--fix` restores network settings a crash left behind. |
| `bckp notify test` | Send a test alert to every backend. |
| `bckp config init\|path\|show` | The config file. |
| `bckp simulate "nominal x4 serious x45"` | Dry-run the safety rules on scripted readings. |

Flags for a session: `-y` (no questions), `-d` (start in the background),
`-f` (run in this terminal), `--no-tui`, `--hotspot=auto|on|off`,
`--watchdog` / `--no-watchdog`. `bckp help` lists everything, and
`bckp COMMAND --help` shows a command's flags.

```
$ bckp status
bckp: awake until 04:42 · hotspot auto · watchdog on (pid 4242)
  options   duration 4h (flag) · hotspot auto (prompt) · watchdog on (prompt)
  session   started 00:42, 3h45m left
  thermal   NOMINAL (CPU 49°C, GPU 47°C)
  power     100% on AC, lid closed
  network   hotspot auto (waiting for the phone), online via Wi-Fi
  lease     held, expires 00:58:54
  kernel    SleepDisabled=Yes, lid closed
```

**From a phone or an agent.** Without a terminal (ssh from a phone, an
agent's shell tool), backpack asks nothing. It starts the session in the
background, prints one line on stdout and returns right away:

```
$ ssh mac bckp 3h
bckp: awake until 18:40 · hotspot auto · watchdog on (pid 4242)
```

If a session is already running, it prints the same line prefixed with
`already running;` and leaves the session as it is.

**How a session ends.** `bckp stop`, `s` or Ctrl-C in the dashboard, or
the end of the requested time end it quietly. A logout, a `kill`, or
closing the terminal of a `-f` session (SIGTERM or SIGHUP) also ends it,
but you get an alert, since you didn't ask for it.

## Phone alerts (recommended)

With the lid closed you won't see macOS banners, so heat warnings reach you
only on your phone:

1. Install the [ntfy](https://ntfy.sh) app. No account is needed.
2. Run `bckp` in a terminal. The first run offers to generate a random
   topic; or set `notify.ntfy_topic` in the config.
3. Subscribe to that topic in the app.
4. Check it with `bckp notify test`.

Anyone who knows the topic can read it, so treat it like a password.
Generated topics are 24 random characters.

Only things that need you are pushed:

- heat warnings, and an upcoming sleep (cancel it with `bckp abort`)
- switching to battery, and a low battery
- the 12h cap coming up
- a session ending unexpectedly, or while on battery
- anything failing: a lost lease, bad sensor readings, a failed network
  restore, still offline after 3 tries
- "back online" after a long outage

Everything else is only logged (`bckp logs`).

## Hotspot (optional, iPhone)

- **USB:** plug the iPhone in and turn on Personal Hotspot. With `hotspot
  auto` (the default), backpack puts the iPhone ahead of Wi-Fi once it's
  connected. If the link drops, backpack turns the service off and on again
  to recover it. The original network order is restored when the session
  ends.
- **Over Wi-Fi:** in System Settings → Wi-Fi, set **Ask to join hotspots**
  to **Automatic**, and start with `bckp --hotspot=on`. When the Mac has
  been offline for a minute, backpack cycles Wi-Fi so macOS joins the
  iPhone's hotspot. This works even with the phone locked in a pocket.
  backpack never does this while the Mac is online.

## Safety model

- **The root helper can do exactly three things:** disable lid-close sleep,
  re-enable it, and sleep now. Its commands are hard-coded; it takes no
  arguments from clients, and only your user can talk to it.
- **Crash-safe:** sleep is only disabled while backpack holds a lease on
  the helper's socket. If backpack crashes or is `kill -9`ed, the helper
  re-enables sleep at once; in testing that took 79 ms. A hung backpack
  loses the lease within 90s. The helper also resets the setting when it
  starts, so after a reboot the Mac sleeps normally again.
- **Heat backstop:** backpack sleeps the Mac after 3 minutes of serious
  heat (30s warning; cancel with `bckp abort`) or at critical heat (15s,
  no cancelling).
- **Battery heat:** in a closed bag the case and battery can heat up while
  the chip stays cool enough that macOS still reports "nominal". backpack
  reads the battery's own temperature sensor and warns at 40°C, sleeps at
  45°C (30s, cancellable) and at 50°C (15s, no cancelling). Apple warns
  that heat above 35°C can permanently reduce battery capacity; the battery
  runs about 10°C warmer than the room, so 45°C is roughly a 35°C room.
- **Battery backstop:** backpack ends the session at 25% battery. The
  helper itself forces sleep on repeated critical heat, a battery at 50°C
  or under 10% charge, even if backpack is stuck.
- **Auto-restore:** sessions are capped at 12h (`extend` restarts the
  cap). Network changes are journaled and undone at the end, even after a
  crash.

Details: [docs/DESIGN.md](docs/DESIGN.md).

## Configuration

`~/.config/backpack/config.toml`. The first run in a terminal creates it,
or run `bckp config init`. It only sets what you change; everything else
keeps its default. `bckp config show` prints the effective config (the
ntfy topic masked). Every key can be overridden with an
environment variable, `BACKPACK_<SECTION>_<KEY>`.

```toml
[session]
max_duration = "12h"        # cap on every session, even "forever"

[thermal]
battery_warn_c = 40         # battery °C: warn
battery_sleep_c = 45        # battery °C: sleep (cancellable)
battery_critical_c = 50     # battery °C: sleep (not cancellable; max 50)

[power]
battery_floor = 25          # end the session at this battery %

[network]
prefer_hotspot = "auto"     # "auto" | "on" | "off"
watchdog = true             # watch the network, alert on outages
service = "iPhone USB"      # the hotspot's network service name

[notify]
ntfy_topic = ""             # your ntfy topic, for phone alerts
macos = true                # macOS notifications
push = "attention"          # "all" pushes routine events too
```

## Known limitations

- macOS only. Release binaries are universal (Apple Silicon and Intel) and
  need macOS 12 or later, but it has been tested on one machine only:
  Apple Silicon, macOS 27.
- English only. The hotspot service is looked up as "iPhone USB"; on a
  non-English macOS it may be named differently, so set `network.service`.
- Hotspot features are for iPhone only.
- backpack can't join a Wi-Fi network by name: without Location Services
  permission, macOS hides network names from command-line tools. It can
  only nudge macOS to auto-join. Networks the Mac already knows (home, a
  café) win over the hotspot; turn off Auto-Join for them if you want the
  hotspot instead.
- CPU and GPU °C need [macmon](https://github.com/vladkens/macmon) (`brew
  install macmon`) on Apple Silicon; they are shown, and used only by the
  optional `thermal.cpu_sleep_c`. Battery °C needs no extra install.
- Battery °C comes from a private macOS interface (the same one macmon and
  iStat Menus use). If a macOS update breaks it, backpack notifies you
  ("Battery temperature unavailable") and keeps running on the thermal
  state rules alone.
- Network features need an admin account (`networksetup` without sudo).
- One user per Mac: installing the helper as another user replaces the
  allowed user.
- While the helper is installed, it owns the sleep setting: a manual
  `sudo pmset -a disablesleep 1` is undone within a minute.
- macOS banners come from `osascript`. If none appear, allow notifications
  for Script Editor in System Settings → Notifications. A session started
  over ssh may not be able to show banners at all; use ntfy.
- The binaries are not signed or notarized. The installer uses curl, which
  doesn't set the quarantine flag; a tarball downloaded in a browser is
  blocked by Gatekeeper.
- A bag still gets hot. backpack warns you and sleeps the Mac, but it can't
  cool it. Give the Mac some air, and avoid heavy work in a closed bag.

## Uninstall

```sh
curl -fsSL https://raw.githubusercontent.com/ilias-t/bckp/main/install.sh | sh -s -- --uninstall
```

This runs `sudo backpack uninstall`, which re-enables sleep and removes the
helper, then deletes the binaries. For a source install, run `sudo backpack
uninstall` yourself. Config and logs stay in `~/.config/backpack` and
`~/Library/Logs/backpack`, and session state in `~/Library/Application
Support/backpack`; delete them if you like.

## License

MIT. See [LICENSE](LICENSE). Contributions: [CONTRIBUTING.md](CONTRIBUTING.md).

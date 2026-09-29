// Package paths centralizes every filesystem location backpack uses.
//
// System (root-owned) locations are constants. Per-user locations are
// derived from a home directory via ForHome so tests can use a temp home.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// System locations (installed by `sudo backpack install`).
const (
	// DaemonSocket is backpackd's socket: root:wheel, mode 0666. The peer
	// uid allowlist, not file permissions, gates access.
	DaemonSocket = "/var/run/backpackd.sock"
	// DaemonBinary is where install copies backpackd: root:wheel 0755.
	DaemonBinary = "/Library/PrivilegedHelperTools/backpackd"
	// DaemonLabel is the LaunchDaemon label.
	DaemonLabel = "dev.bckp.helper"
	// DaemonPlist is the LaunchDaemon plist.
	DaemonPlist = "/Library/LaunchDaemons/dev.bckp.helper.plist"
	// SystemSupportDir holds root-owned daemon configuration.
	SystemSupportDir = "/Library/Application Support/backpack"
	// AllowedUIDs lists the uids (one decimal uid per line) allowed to
	// talk to the daemon; root-owned, written at install.
	AllowedUIDs = "/Library/Application Support/backpack/allowed_uids"
	// DaemonLogDir receives backpackd's logs (the plist's stdout/stderr);
	// root-owned, created by install because launchd does not create it.
	DaemonLogDir = "/Library/Logs/backpack"
)

// Absolute paths of the system tools backpack shells out to. Always use
// these (never PATH lookup) so that root never runs a user-writable binary
// and fakes.Exec keys match real invocations.
const (
	BinPmset        = "/usr/bin/pmset"
	BinIoreg        = "/usr/sbin/ioreg"
	BinNetworksetup = "/usr/sbin/networksetup"
	BinRoute        = "/sbin/route"
	BinIfconfig     = "/sbin/ifconfig"
	BinPing         = "/sbin/ping"
	BinDig          = "/usr/bin/dig"
	BinScutil       = "/usr/sbin/scutil"
	BinLaunchctl    = "/bin/launchctl"
	BinDseditgroup  = "/usr/sbin/dseditgroup"
	BinOsascript    = "/usr/bin/osascript"
	// BinSudo runs `sudo backpack install` from the first-run offer, with
	// the terminal attached so sudo can ask for the password.
	BinSudo = "/usr/bin/sudo"
)

// MacmonCandidates are the locations searched for macmon (user engine
// only; never run by root), in order.
var MacmonCandidates = []string{"/opt/homebrew/bin/macmon", "/usr/local/bin/macmon"}

// MaxSocketPathLen is the longest usable Unix socket path on macOS:
// sockaddr_un.sun_path is 104 bytes including the terminating NUL. Note
// that t.TempDir() on macOS lives under /var/folders/... and can exceed
// this when nested; tests that bind sockets should use a short dir (e.g.
// os.MkdirTemp("/tmp", "bckp")).
const MaxSocketPathLen = 103

// CheckSocketPath returns an error when p is too long to bind.
func CheckSocketPath(p string) error {
	if len(p) > MaxSocketPathLen {
		return fmt.Errorf("paths: socket path is %d bytes, max %d: %s", len(p), MaxSocketPathLen, p)
	}
	return nil
}

// User holds per-user locations.
type User struct {
	Home string
	// AppSupport is ~/Library/Application Support/backpack.
	AppSupport string
	// EngineSocket is AppSupport/engine.sock (mode 0600).
	EngineSocket string
	// EngineLock is AppSupport/engine.lock (flock for single instance).
	EngineLock string
	// RestoreJournal is AppSupport/restore.json (network restore journal).
	RestoreJournal string
	// OutboxDir is AppSupport/outbox (disk-backed notification outbox).
	OutboxDir string
	// LogDir is ~/Library/Logs/backpack.
	LogDir string
	// EngineLog is LogDir/engine.log (detached engine stdio).
	EngineLog string
	// ConfigDir is ~/.config/backpack.
	ConfigDir string
	// ConfigFile is ConfigDir/config.toml.
	ConfigFile string
}

// ForHome derives per-user locations from home.
func ForHome(home string) User {
	appSupport := filepath.Join(home, "Library", "Application Support", "backpack")
	logDir := filepath.Join(home, "Library", "Logs", "backpack")
	configDir := filepath.Join(home, ".config", "backpack")
	return User{
		Home:           home,
		AppSupport:     appSupport,
		EngineSocket:   filepath.Join(appSupport, "engine.sock"),
		EngineLock:     filepath.Join(appSupport, "engine.lock"),
		RestoreJournal: filepath.Join(appSupport, "restore.json"),
		OutboxDir:      filepath.Join(appSupport, "outbox"),
		LogDir:         logDir,
		EngineLog:      filepath.Join(logDir, "engine.log"),
		ConfigDir:      configDir,
		ConfigFile:     filepath.Join(configDir, "config.toml"),
	}
}

// Current returns per-user locations for the current user's home.
func Current() (User, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return User{}, fmt.Errorf("paths: home dir: %w", err)
	}
	return ForHome(home), nil
}

package daemon

import _ "embed"

// launchdPlist is the LaunchDaemon definition, the only copy in the repo.
// ExitTimeOut gives the SIGTERM path (disablesleep 0, verified and retried)
// up to 60s before launchd sends SIGKILL.
//
//go:embed dev.bckp.helper.plist
var launchdPlist []byte

// LaunchdPlist returns the LaunchDaemon plist that `backpack install`
// writes to paths.DaemonPlist.
func LaunchdPlist() []byte { return append([]byte(nil), launchdPlist...) }

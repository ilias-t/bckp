// Package version reports the build's version.
package version

import "runtime/debug"

// Version is set at release build time:
//
//	go build -ldflags "-X github.com/ilias-t/bckp/internal/version.Version=v1.2.3"
var Version string

// String is Version when set, else the module version from the build info
// (the tag for `go install ...@v1.2.3`), else "dev" plus the VCS revision
// when the build has one.
func String() string {
	if Version != "" {
		return Version
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	v := "dev"
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 12 {
			v += "+" + s.Value[:12]
		}
	}
	return v
}

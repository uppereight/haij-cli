package main

import (
	"runtime"
	"runtime/debug"
	"strings"
)

// goVersion returns the Go runtime version that built this binary
// (e.g. "go1.22.0"). Used in `haij --version` output (spec test #87).
func goVersion() string {
	return runtime.Version()
}

// osArch returns "GOOS/GOARCH" (e.g. "darwin/arm64") for the current
// process. Used in `haij --version`.
func osArch() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

// versionInfo returns the version and commit `haij --version` reports.
func versionInfo() (string, string) {
	info, _ := debug.ReadBuildInfo()
	return resolveVersion(version, commit, info)
}

// resolveVersion prefers the -ldflags values release builds set
// (scripts/release.sh). A binary from `go install …@v0.1.0` has none,
// but Go records the module version, and a build from a git checkout
// records the revision, so fall back to those instead of reporting
// 0.0.0-dev / unknown.
func resolveVersion(v, c string, info *debug.BuildInfo) (string, string) {
	if info == nil {
		return v, c
	}
	if v == "0.0.0-dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		v = strings.TrimPrefix(info.Main.Version, "v")
	}
	if c == "unknown" {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				c = s.Value[:7]
			}
		}
	}
	return v, c
}

package main

import "runtime/debug"

// version is the module version baked into the binary: `go install
// ...@v0.1.0` records "v0.1.0". A build from a local checkout has no release
// version, so it says "devel" (newer Go versions may instead record a
// pseudo-version with the commit hash, which is just as useful in a bug
// report). It is never empty.
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return versionName("")
	}
	return versionName(info.Main.Version)
}

// versionName turns the recorded version into what we print. It is separate
// from version() so a test can feed it each case.
func versionName(recorded string) string {
	if recorded == "" || recorded == "(devel)" {
		return "devel"
	}
	return recorded
}

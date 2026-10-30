package main

import "testing"

func TestVersionName(t *testing.T) {
	for _, c := range []struct{ recorded, want string }{
		{"v0.1.0", "v0.1.0"},
		{"v0.2.0-rc.1", "v0.2.0-rc.1"},
		{"v0.0.0-20250101000000-abcdef123456+dirty", "v0.0.0-20250101000000-abcdef123456+dirty"},
		{"(devel)", "devel"}, // what `go build` records for a local build
		{"", "devel"},        // no build info at all
	} {
		if got := versionName(c.recorded); got != c.want {
			t.Errorf("versionName(%q) = %q, want %q", c.recorded, got, c.want)
		}
	}
}

// version() must never be empty, whatever the binary was built from (under
// `go test` the recorded version is "(devel)" or a pseudo-version).
func TestVersionNotEmpty(t *testing.T) {
	if version() == "" {
		t.Error("version() is empty")
	}
}

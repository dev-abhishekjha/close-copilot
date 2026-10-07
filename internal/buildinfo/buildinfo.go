// Package buildinfo reports the version every binary prints.
package buildinfo

import (
	"runtime/debug"
	"strings"
)

// Version is set at build time:
//
//	go build -ldflags "-X github.com/abhishekjha/close-copilot/internal/buildinfo.Version=v0.1.0" ./cmd/...
//
// Without it, String falls back to the VCS revision Go embeds in the binary.
var Version = ""

// String returns the build version, the short VCS revision, or "dev".
func String() string {
	if Version != "" {
		return Version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	var b strings.Builder
	b.WriteString("dev-")
	b.WriteString(rev)
	if dirty {
		b.WriteString("-dirty")
	}
	return b.String()
}

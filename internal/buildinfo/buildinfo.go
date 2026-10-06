// Package buildinfo reports the running build's version.
//
// Release builds set these with -ldflags, e.g.
//
//	-X certsforever/internal/buildinfo.Version=v1.2.0
//	-X certsforever/internal/buildinfo.Commit=abc1234
//
// Otherwise the VCS revision embedded by `go build` is used when available.
package buildinfo

import "runtime/debug"

var (
	Version = "dev"
	Commit  = ""
)

// Info is the build identity.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	Go      string `json:"go"`
}

// Get returns the build identity.
func Get() Info {
	i := Info{Version: Version, Commit: Commit}
	if bi, ok := debug.ReadBuildInfo(); ok {
		i.Go = bi.GoVersion
		if i.Commit == "" {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" && len(s.Value) >= 7 {
					i.Commit = s.Value[:7]
				}
			}
		}
	}
	return i
}

func (i Info) String() string {
	if i.Commit == "" {
		return i.Version
	}
	return i.Version + " (" + i.Commit + ")"
}

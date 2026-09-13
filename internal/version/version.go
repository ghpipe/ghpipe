// Package version carries build-time version information.
package version

// Version is injected at build time:
//
//	go build -ldflags "-X github.com/ghpipe/ghpipe/internal/version.Version=0.1.0"
//
// It must stay a bare semver (no leading "v") because the npm wrapper parses
// `ghpipe --version` output of the form "ghpipe <semver>".
var Version = "0.0.0-dev"

// Command is the executable name reported to users and to the wrapper.
const Command = "ghpipe"

// String returns the version protocol line: "ghpipe <semver>".
func String() string { return Command + " " + Version }

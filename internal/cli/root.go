// Package cli owns the command tree, global flags and the mapping from command
// outcomes to exit codes.
package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ghpipe/ghpipe/internal/project"
	"github.com/ghpipe/ghpipe/internal/result"
	"github.com/ghpipe/ghpipe/internal/version"
)

// Streams keeps stdout/stderr injectable for tests.
type Streams struct {
	Out io.Writer
	Err io.Writer
}

// Run parses arguments and executes one command. It returns the process exit
// code (see internal/result).
func Run(args []string, streams Streams) int {
	if streams.Out == nil {
		streams.Out = os.Stdout
	}
	if streams.Err == nil {
		streams.Err = os.Stderr
	}
	if len(args) == 0 {
		printUsage(streams.Err)
		return result.ExitUsageError
	}

	global, rest, err := parseGlobal(args)
	if err != nil {
		return usageFailure(streams, rest, err)
	}
	if len(rest) == 0 {
		printUsage(streams.Err)
		return result.ExitUsageError
	}

	switch rest[0] {
	case "version", "--version", "-v":
		fmt.Fprintln(streams.Out, version.String())
		return result.ExitOK
	case "inspect":
		return runInspect(global, streams)
	case "help", "--help", "-h":
		printUsage(streams.Out)
		return result.ExitOK
	default:
		fmt.Fprintf(streams.Err, "unknown command %q\n", rest[0])
		printUsage(streams.Err)
		return result.ExitUsageError
	}
}

// Global holds the flags accepted before or after a subcommand.
type Global struct {
	Project string
	Config  string
	JSON    bool
}

func parseGlobal(args []string) (Global, []string, error) {
	var g Global
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		takeValue := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("missing value for %s", arg)
			}
			i++
			return args[i], nil
		}
		switch {
		case arg == "--json":
			g.JSON = true
		case arg == "--project":
			v, err := takeValue()
			if err != nil {
				return g, rest, err
			}
			g.Project = v
		case strings.HasPrefix(arg, "--project="):
			g.Project = strings.TrimPrefix(arg, "--project=")
		case arg == "--config":
			v, err := takeValue()
			if err != nil {
				return g, rest, err
			}
			g.Config = v
		case strings.HasPrefix(arg, "--config="):
			g.Config = strings.TrimPrefix(arg, "--config=")
		default:
			rest = append(rest, arg)
		}
	}
	return g, rest, nil
}

func usageFailure(streams Streams, rest []string, err error) int {
	command := "cli"
	if len(rest) > 0 {
		command = rest[0]
	}
	env := result.UsageError(command, err.Error())
	if streams.Err != nil {
		fmt.Fprintf(streams.Err, "%s\n", env.String())
	}
	return env.ExitCode()
}

func runInspect(global Global, streams Streams) int {
	env := result.New("inspect", result.StatusSucceeded)
	start := global.Project
	if start == "" {
		cwd, err := os.Getwd()
		if err != nil {
			env.Fail("local", "precondition").Error.Detail = err.Error()
			return finish(env, global, streams)
		}
		start = cwd
	}
	proj, err := project.Load(start)
	if err != nil {
		// Fall back to discovery so inspect works from a subdirectory.
		if proj, err = project.Discover(start); err != nil {
			env.Fail("configuration", "precondition").Error.Detail = err.Error()
			return finish(env, global, streams)
		}
	}
	env.Repository = proj.Config.Repository
	env.Data = map[string]any{
		"project":         proj.Root,
		"config":          proj.Path,
		"cli_version":     version.Version,
		"schema_version":  proj.Config.SchemaVersion,
		"repository":      proj.Config.Repository,
		"default_branch":  proj.Config.DefaultBranch,
		"quality_mode":    proj.Config.Mode(),
		"commands":        proj.Config.CommandNames(),
		"required_checks": proj.Config.CI.RequiredChecks,
		"reporting":       proj.Config.Reporting.Enabled,
	}
	return finish(env, global, streams)
}

func finish(env *result.Envelope, global Global, streams Streams) int {
	if global.JSON {
		if err := env.WriteJSON(streams.Out); err != nil {
			fmt.Fprintf(streams.Err, "cannot write result: %v\n", err)
			return result.ExitFailed
		}
		return env.ExitCode()
	}
	if env.Error != nil {
		fmt.Fprintf(streams.Err, "%s\n", env.String())
		return env.ExitCode()
	}
	fmt.Fprintf(streams.Out, "%s\n", version.String())
	if data, ok := env.Data.(map[string]any); ok {
		fmt.Fprintf(streams.Out, "project: %v\nconfig: %v\nquality: %v\ncommands: %v\n",
			data["project"], data["config"], data["quality_mode"], data["commands"])
	}
	return env.ExitCode()
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, `%s

Usage:
  ghpipe [--json] [--project DIR] [--config FILE] <command> [flags]

Commands:
  version                 print "ghpipe <semver>"
  inspect                 show the discovered project and its configuration
  help                    show this message

This build is an early skeleton: only version and inspect are implemented.
`, version.String())
}

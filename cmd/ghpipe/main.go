// Command ghpipe is the single entry point of the tool.
package main

import (
	"os"

	"github.com/ghpipe/ghpipe/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], cli.Streams{Out: os.Stdout, Err: os.Stderr}))
}

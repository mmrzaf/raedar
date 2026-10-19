// Command raedar is the Raedar monitoring service.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	_ "time/tzdata" // The distroless image has no zoneinfo files.
)

// Exit codes returned by run.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

const usage = `usage: raedar [command]

commands:
  serve         run the service (default)
  check         validate the configuration and every dependency
  notify-test   send a test message to the notification channels
  healthcheck   ask the local health endpoint whether Raedar is ready
  version       print the version
`

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches one command line and returns the process exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return runServe(ctx, nil, stdout, stderr)
	}
	command, rest := args[0], args[1:]
	switch command {
	case "serve":
		return runServe(ctx, rest, stdout, stderr)
	case "check":
		return runCheck(ctx, rest, stdout, stderr)
	case "healthcheck":
		return runHealthcheck(ctx, rest, stdout, stderr)
	case "notify-test":
		return runNotifyTest(ctx, rest, stdout, stderr)
	case "version":
		return runVersion(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "raedar: unknown command %q\n\n%s", command, usage)
		return exitUsage
	}
}

// runVersion prints the version injected at build time.
func runVersion(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprint(stderr, "raedar: version takes no arguments\n")
		return exitUsage
	}
	fmt.Fprintf(stdout, "raedar %s\n", version)
	return exitOK
}

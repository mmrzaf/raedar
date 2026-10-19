package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/notify"
)

// runNotifyTest sends a test message through the configured channels, or
// through one channel.
func runNotifyTest(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	channel, problem := parseChannel(args)
	if problem != "" {
		fmt.Fprintf(stderr, "raedar: %s\n", problem)
		return exitUsage
	}
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		fmt.Fprintf(stderr, "raedar: invalid configuration:\n%v\n", err)
		return exitError
	}
	logger := slog.New(slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	results, err := notify.Test(ctx, cfg, logger, notify.SystemClock(), channel)
	if err != nil {
		fmt.Fprintf(stderr, "raedar: %v\n", err)
		return exitError
	}
	if len(results) == 0 {
		fmt.Fprintln(stdout, "no channels are configured")
		return exitError
	}
	failed := false
	for _, result := range results {
		if result.Err != nil {
			failed = true
			fmt.Fprintf(stdout, "%s: failed: %v\n", result.Channel, result.Err)
			continue
		}
		fmt.Fprintf(stdout, "%s: sent\n", result.Channel)
	}
	if failed {
		return exitError
	}
	return exitOK
}

// parseChannel reads the optional --channel flag.
func parseChannel(args []string) (string, string) {
	channel := ""
	for i := 0; i < len(args); i++ {
		argument := args[i]
		switch {
		case argument == "--channel":
			if i+1 >= len(args) {
				return "", "--channel needs a value"
			}
			channel = args[i+1]
			i++
		case strings.HasPrefix(argument, "--channel="):
			channel = strings.TrimPrefix(argument, "--channel=")
		default:
			return "", fmt.Sprintf("unknown argument %q", argument)
		}
	}
	switch channel {
	case "", "telegram", "sms", "webhook":
		return channel, ""
	}
	return "", fmt.Sprintf("unknown channel %q: want telegram, sms or webhook", channel)
}

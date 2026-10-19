package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"

	"github.com/mmrzaf/raedar/internal/config"
)

// runHealthcheck asks the local health endpoint whether Raedar is ready.
func runHealthcheck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprint(stderr, "raedar: healthcheck takes no arguments\n")
		return exitUsage
	}
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		fmt.Fprintf(stderr, "raedar: invalid configuration:\n%v\n", err)
		return exitError
	}
	client := &http.Client{Timeout: checkTimeout}
	if err := healthCheck(ctx, cfg.Listen, client); err != nil {
		fmt.Fprintf(stderr, "raedar: healthcheck: %v\n", err)
		return exitError
	}
	fmt.Fprintln(stdout, "ok")
	return exitOK
}

// healthCheck calls the health endpoint on the local listener.
func healthCheck(ctx context.Context, listen string, client *http.Client) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+healthAddress(listen)+"/healthz", nil)
	if err != nil {
		return fmt.Errorf("build the request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16)); err != nil {
		return fmt.Errorf("read the response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("the health endpoint answered %s", response.Status)
	}
	return nil
}

// healthAddress returns the address the health endpoint is reachable at from
// inside the container or host.
func healthAddress(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

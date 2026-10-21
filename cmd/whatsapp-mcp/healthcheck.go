package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// runHealthcheck probes a running serve daemon's /healthz (or /readyz with
// -ready) and exits 0 on HTTP 200, 1 otherwise. It lets a container
// HEALTHCHECK probe the daemon without curl or wget in the image.
func runHealthcheck(args []string) int {
	var (
		addr    string
		ready   bool
		timeout time.Duration
	)
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&addr, "addr", "", "address the daemon listens on (default: 127.0.0.1:8765, env WHATSAPP_MCP_ADDR)")
	fs.BoolVar(&ready, "ready", false, "probe /readyz (paired and connected to WhatsApp) instead of /healthz")
	fs.DurationVar(&timeout, "timeout", 5*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	path := "/healthz"
	if ready {
		path = "/readyz"
	}
	url := "http://" + probeAddr(resolveAddr(addr)) + path

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	fmt.Fprintf(os.Stdout, "%s %d %s\n", path, resp.StatusCode, strings.TrimSpace(string(body)))
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

// probeAddr turns a bind address into one a local client can dial: an
// unspecified host (0.0.0.0, ::, or empty) becomes the loopback address of
// the same family.
func probeAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch ip := net.ParseIP(host); {
	case host == "":
		host = "127.0.0.1"
	case ip != nil && ip.IsUnspecified() && ip.To4() == nil:
		host = "::1"
	case ip != nil && ip.IsUnspecified():
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

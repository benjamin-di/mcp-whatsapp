package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/sealjay/mcp-whatsapp/internal/client"
	"github.com/sealjay/mcp-whatsapp/internal/daemon"
	mcpsrv "github.com/sealjay/mcp-whatsapp/internal/mcp"
	"github.com/sealjay/mcp-whatsapp/internal/security"
	"github.com/sealjay/mcp-whatsapp/internal/store"
)

func runServe(storeDir string, redactor *security.Redactor, args []string) int {
	var (
		addr        string
		allowRemote bool
		ad          autoDownloadFlags
	)
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&addr, "addr", "", "HTTP bind address (default: 127.0.0.1:8765, env WHATSAPP_MCP_ADDR)")
	fs.BoolVar(&allowRemote, "allow-remote", false, "allow binding to a non-loopback address")
	fs.StringVar(&ad.types, "auto-download", "", "download incoming media of these types automatically: comma list of image,video,audio,document, or all (default: off, env WHATSAPP_MCP_AUTO_DOWNLOAD)")
	fs.IntVar(&ad.maxMB, "auto-download-max-mb", client.DefaultAutoDownloadMaxMB, "skip auto-download of media larger than this many MiB; 0 = no limit (env WHATSAPP_MCP_AUTO_DOWNLOAD_MAX_MB)")
	fs.IntVar(&ad.backfillDays, "auto-download-backfill-days", 0, "only backfill stored media from the last N days; 0 = no limit (env WHATSAPP_MCP_AUTO_DOWNLOAD_BACKFILL_DAYS)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "auto-download":
			ad.typesSet = true
		case "auto-download-max-mb":
			ad.maxMBSet = true
		case "auto-download-backfill-days":
			ad.backfillDaysSet = true
		}
	})
	autoDownloadCfg, err := resolveAutoDownload(ad, os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 2
	}

	addr = resolveAddr(addr)
	if !allowRemote && !isLoopbackAddr(addr) {
		fmt.Fprintf(os.Stderr, "refusing to bind to non-loopback address %q; pass -allow-remote if you mean it\n", addr)
		return 2
	}

	// When binding to a non-loopback address we require a shared bearer
	// token. Loopback-only operation intentionally keeps no token so local
	// editors and curl can hit the daemon without extra setup.
	var authToken string
	if allowRemote {
		authToken = os.Getenv("WHATSAPP_MCP_TOKEN")
		if authToken == "" {
			fmt.Fprintln(os.Stderr, "-allow-remote requires WHATSAPP_MCP_TOKEN to be set in the environment")
			return 2
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	lock, err := store.TryLock(storeDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	defer lock.Release()

	absStore, err := filepath.Abs(storeDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve store dir: %v\n", err)
		return 1
	}
	allowedMediaRoot := os.Getenv("WHATSAPP_MCP_MEDIA_ROOT")
	if allowedMediaRoot == "" {
		allowedMediaRoot = filepath.Join(absStore, "uploads")
	}
	allowedMediaRoot = filepath.Clean(allowedMediaRoot)
	if mkErr := os.MkdirAll(allowedMediaRoot, 0o755); mkErr != nil {
		fmt.Fprintf(os.Stderr, "warn: could not create media root %q: %v\n", allowedMediaRoot, mkErr)
	}
	// Operator UX: make the media root obvious on boot so users know where
	// to drop outbound attachments.
	fmt.Fprintf(os.Stderr, "media root: %s (drop outbound files here)\n", allowedMediaRoot)

	st, err := store.Open(storeDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open store: %v\n", err)
		return 1
	}
	defer st.Close()

	c, err := client.New(ctx, client.Config{
		StoreDir:         storeDir,
		Store:            st,
		Logger:           client.NewStderrLogger("Client", "INFO", false),
		AllowedMediaRoot: allowedMediaRoot,
		Redactor:         redactor,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "init client: %v\n", err)
		return 1
	}

	// Start the download workers before the driver connects, so the event
	// handler sees them from the first message on. Stopping them is
	// deferred after st.Close, so it runs first on shutdown.
	stopAutoDownload := c.StartAutoDownload(ctx, autoDownloadCfg)
	defer stopAutoDownload()
	if autoDownloadCfg.Enabled() {
		limit := "no size limit"
		if autoDownloadCfg.MaxBytes > 0 {
			limit = fmt.Sprintf("up to %d MiB", autoDownloadCfg.MaxBytes>>20)
		}
		backfill := "all stored media"
		if autoDownloadCfg.BackfillDays > 0 {
			backfill = fmt.Sprintf("last %d days", autoDownloadCfg.BackfillDays)
		}
		fmt.Fprintf(os.Stderr, "auto-download: %s (%s; backfill: %s)\n", strings.Join(autoDownloadCfg.TypeList(), ","), limit, backfill)
	}

	drv := newProductionDriver(c)

	// The daemon owns the pair cache, so build it first, then hand the cache
	// to the MCP server (so pairing_status can surface the live QR), then
	// mount the MCP HTTP handler back onto the daemon.
	d, err := daemon.New(daemon.Config{
		Addr:      addr,
		Driver:    drv,
		AuthToken: authToken,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon.New: %v\n", err)
		return 1
	}

	mcpServer := mcpsrv.NewServer(c, d.Cache())
	d.SetMCPMount(mcpServer.AttachHTTP)

	fmt.Fprintf(os.Stderr, "whatsapp-mcp listening on http://%s (MCP at /mcp, pairing at /pair)\n", addr)
	if !c.IsLoggedIn() {
		fmt.Fprintf(os.Stderr, "unpaired: open http://%s/pair to scan QR\n", addr)
	}
	if err := d.Run(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "daemon: %v\n", err)
		return 1
	}
	return 0
}

// resolveAddr applies the -addr / WHATSAPP_MCP_ADDR / default precedence.
func resolveAddr(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv("WHATSAPP_MCP_ADDR"); env != "" {
		return env
	}
	return "127.0.0.1:8765"
}

// autoDownloadFlags holds the -auto-download* flag values and whether each
// was set on the command line.
type autoDownloadFlags struct {
	types           string
	typesSet        bool
	maxMB           int
	maxMBSet        bool
	backfillDays    int
	backfillDaysSet bool
}

// resolveAutoDownload applies the flag / env / default precedence for the
// -auto-download* flags. A flag that was set on the command line wins over
// its env var, even when set to the empty string.
func resolveAutoDownload(f autoDownloadFlags, getenv func(string) string) (client.AutoDownloadConfig, error) {
	spec := f.types
	if !f.typesSet {
		spec = getenv("WHATSAPP_MCP_AUTO_DOWNLOAD")
	}
	types, err := client.ParseAutoDownloadTypes(spec)
	if err != nil {
		return client.AutoDownloadConfig{}, fmt.Errorf("auto-download: %w", err)
	}

	maxMB := f.maxMB
	if !f.maxMBSet {
		if env := strings.TrimSpace(getenv("WHATSAPP_MCP_AUTO_DOWNLOAD_MAX_MB")); env != "" {
			maxMB, err = strconv.Atoi(env)
			if err != nil {
				return client.AutoDownloadConfig{}, fmt.Errorf("WHATSAPP_MCP_AUTO_DOWNLOAD_MAX_MB: %q is not a whole number of MiB", env)
			}
		}
	}
	if maxMB < 0 || uint64(maxMB) > math.MaxUint64>>20 {
		return client.AutoDownloadConfig{}, fmt.Errorf("auto-download max size must be 0 (no limit) or a positive number of MiB, got %d", maxMB)
	}

	days := f.backfillDays
	if !f.backfillDaysSet {
		if env := strings.TrimSpace(getenv("WHATSAPP_MCP_AUTO_DOWNLOAD_BACKFILL_DAYS")); env != "" {
			days, err = strconv.Atoi(env)
			if err != nil {
				return client.AutoDownloadConfig{}, fmt.Errorf("WHATSAPP_MCP_AUTO_DOWNLOAD_BACKFILL_DAYS: %q is not a whole number of days", env)
			}
		}
	}
	// 100 years is far beyond any stored message and keeps AddDate sane.
	if days < 0 || days > 36500 {
		return client.AutoDownloadConfig{}, fmt.Errorf("auto-download backfill days must be 0 (no limit) or a positive number of days up to 36500, got %d", days)
	}
	return client.AutoDownloadConfig{Types: types, MaxBytes: uint64(maxMB) << 20, BackfillDays: days}, nil
}

// isLoopbackAddr reports whether addr's host resolves to a loopback IP.
// Hostnames (including "localhost") are resolved via DNS; all returned
// addresses must be loopback for the check to pass. Non-loopback binds
// require -allow-remote.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	// If it parses directly as an IP, check without DNS.
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	// Resolve hostname and require every address to be loopback.
	addrs, err := net.LookupHost(host)
	if err != nil || len(addrs) == 0 {
		return false
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil || !ip.IsLoopback() {
			return false
		}
	}
	return true
}

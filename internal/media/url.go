package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxURLBytes = 64 << 20

// DownloadURL stages a public HTTPS file for a single send. The caller must run
// cleanup after sending, including when the send fails. No proxy, cookies or
// credentials are used. Errors deliberately omit URLs, which may contain tokens.
func DownloadURL(ctx context.Context, sourceURL, root string) (path string, cleanup func(), err error) {
	transport := &http.Transport{
		DialContext:           publicDial(net.DefaultResolver.LookupIPAddr, (&net.Dialer{Timeout: 10 * time.Second}).DialContext),
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		DisableKeepAlives:     true,
	}
	defer transport.CloseIdleConnections()
	return downloadURL(ctx, sourceURL, root, mediaHTTPClient(transport))
}

func mediaHTTPClient(transport http.RoundTripper) *http.Client {
	return &http.Client{
		Transport: transport,
		Timeout:   60 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// net/http otherwise adds the previous URL as Referer. File links
			// can carry credentials in their path or query, so never forward it.
			req.Header.Del("Referer")
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			if len(via) > 3 {
				return errors.New("too many media redirects")
			}
			return checkMediaURL(req.URL)
		},
	}
}

func checkMediaURL(u *url.URL) error {
	if u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		(u.Port() != "" && u.Port() != "443") || strings.Contains(u.Host, "%") || u.Fragment != "" {
		return errors.New("source_url must be public HTTPS on port 443, without credentials or a fragment")
	}
	return nil
}

// Reject private, local, reserved and address-transition ranges, including
// mapped IPv4; checking every DNS result prevents mixed public/private answers.
var deniedNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/3"), netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"), netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"), netip.MustParsePrefix("fec0::/10"), netip.MustParsePrefix("ff00::/8"),
}

func publicIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range deniedNetworks {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

func publicDial(
	lookup func(context.Context, string) ([]net.IPAddr, error),
	dial func(context.Context, string, string) (net.Conn, error),
) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != "443" {
			return nil, errors.New("media destination must use port 443")
		}
		ips, err := lookup(ctx, host)
		if err != nil || len(ips) == 0 {
			return nil, errors.New("media host could not be resolved")
		}
		for _, ip := range ips {
			if ip.Zone != "" || !publicIP(ip.IP) {
				return nil, errors.New("media host must resolve only to public addresses")
			}
		}
		// Dial the checked address itself: a second DNS lookup cannot rebind it.
		for _, ip := range ips {
			conn, err := dial(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if err == nil {
				return conn, nil
			}
		}
		return nil, errors.New("media connection failed")
	}
}

func downloadURL(ctx context.Context, sourceURL, root string, client *http.Client) (path string, cleanup func(), err error) {
	u, err := url.Parse(sourceURL)
	if err != nil || len(sourceURL) > 8192 {
		return "", nil, errors.New("invalid source_url")
	}
	if err := checkMediaURL(u); err != nil {
		return "", nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", nil, errors.New("invalid source_url")
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, errors.New("media download failed; the link may be expired or inaccessible")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, errors.New("media download did not return a file (HTTP 200 required)")
	}
	if resp.ContentLength > maxURLBytes {
		return "", nil, errors.New("media exceeds the 64 MiB download limit")
	}
	head := make([]byte, 512)
	n, err := io.ReadFull(resp.Body, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", nil, errors.New("media body could not be read")
	}
	head = head[:n]
	kind := http.DetectContentType(head)
	if n == 0 || kind == "text/html; charset=utf-8" {
		return "", nil, errors.New("media URL returned an empty file or HTML page")
	}
	name := filepath.Base(u.Path)
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
		name = filepath.Base(params["filename"])
	}
	// Sniff image extensions so a token-only URL still sends an image, not a BIN document.
	imageExt := map[string]string{
		"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/webp": ".webp",
		"application/pdf": ".pdf", "video/mp4": ".mp4", "video/quicktime": ".mov", "audio/mpeg": ".mp3",
	}
	if ext := imageExt[kind]; ext != "" {
		name = "photo" + ext
	}
	if name == "." || name == ".." || name == "/" || name == "" || strings.ContainsAny(name, "\x00\\\r\n") || len(name) > 200 {
		name = "attachment.bin"
	}
	dir, err := os.MkdirTemp(root, ".media-url-*")
	if err != nil {
		return "", nil, errors.New("cannot stage media under the configured media root")
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	path = filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", cleanup, errors.New("cannot create temporary media file")
	}
	defer f.Close()
	count, err := io.Copy(f, io.LimitReader(io.MultiReader(bytes.NewReader(head), resp.Body), maxURLBytes+1))
	if err != nil {
		return "", cleanup, errors.New("media body could not be downloaded")
	}
	if count > maxURLBytes {
		return "", cleanup, errors.New("media exceeds the 64 MiB download limit")
	}
	if resp.ContentLength >= 0 && count != resp.ContentLength {
		return "", cleanup, errors.New("media body was incomplete")
	}
	if err = f.Close(); err != nil {
		return "", cleanup, errors.New("cannot finish temporary media file")
	}
	return path, cleanup, nil
}

// Package medialink hands out one-time download links for media the daemon
// has already decrypted, so a remote MCP client, or another server acting for
// it, can fetch the bytes without access to the daemon's filesystem.
//
// A link is the daemon's public URL plus /media/<token>. The token is the
// only credential: 32 random bytes, valid for TTL, and spent by the first GET
// whatever its outcome. Links live in memory, so a restart revokes them all.
package medialink

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// PathPrefix is where the daemon serves links.
	PathPrefix = "/media/"
	// TTL is how long an unused link stays valid.
	TTL = 10 * time.Minute
	// maxLinks bounds the unused links outstanding at once.
	maxLinks = 256
	// tokenLen is the length of a base64url-encoded 32-byte token.
	tokenLen = 43
)

// ErrTooMany is returned by Issue while maxLinks unused links are outstanding.
var ErrTooMany = errors.New("too many unused download links; try again once some have been used or expired")

type entry struct {
	path     string
	filename string
	expires  time.Time
}

// Registry issues links under one public base URL and serves each link once.
// It implements http.Handler for PathPrefix.
type Registry struct {
	base string
	now  func() time.Time

	mu    sync.Mutex
	links map[string]entry
}

// New returns a Registry whose links start with publicURL, the address
// clients reach the daemon at (e.g. https://example.com/whatsapp).
func New(publicURL string) (*Registry, error) {
	u, err := url.Parse(strings.TrimSpace(publicURL))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("public URL %q must be an http(s) address without user, query or fragment", publicURL)
	}
	return &Registry{
		base:  strings.TrimRight(u.String(), "/"),
		now:   time.Now,
		links: make(map[string]entry),
	}, nil
}

// Issue returns a one-time link to the file at path, offered for download as
// filename (the base name of path when empty), and when the link expires.
func (r *Registry) Issue(path, filename string) (string, time.Time, error) {
	if filename == "" {
		filename = filepath.Base(path)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", time.Time{}, fmt.Errorf("token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(buf)

	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for t, e := range r.links {
		if !now.Before(e.expires) {
			delete(r.links, t)
		}
	}
	if len(r.links) >= maxLinks {
		return "", time.Time{}, ErrTooMany
	}
	expires := now.Add(TTL)
	r.links[token] = entry{path: path, filename: filename, expires: expires}
	return r.base + PathPrefix + token, expires, nil
}

// lookup returns the live entry for token; spend removes it.
func (r *Registry) lookup(token string, spend bool) (entry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.links[token]
	if !ok {
		return entry{}, false
	}
	if spend || !r.now().Before(e.expires) {
		delete(r.links, token)
	}
	return e, r.now().Before(e.expires)
}

// ServeHTTP serves GET (spends the link) and HEAD (does not) on
// PathPrefix<token>.
func (r *Registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimPrefix(req.URL.Path, PathPrefix)
	if len(token) != tokenLen || strings.Trim(token, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
		http.Error(w, "unknown download link", http.StatusNotFound)
		return
	}
	e, ok := r.lookup(token, req.Method == http.MethodGet)
	if !ok {
		http.Error(w, "download link expired or already used; ask for a new one", http.StatusNotFound)
		return
	}
	f, err := os.Open(e.path)
	if err != nil {
		http.Error(w, "the file is no longer on the server; download the media again", http.StatusGone)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "the file is no longer on the server; download the media again", http.StatusGone)
		return
	}
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", contentType(head[:n], e.filename))
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": e.filename})
	if disposition == "" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition", disposition)
	if req.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, f)
}

// contentType trusts the bytes for media and PDFs (WhatsApp names every photo
// .jpg, even WebP ones) and the file extension for everything else, where
// sniffing sees only a generic container (a .docx sniffs as application/zip).
func contentType(head []byte, filename string) string {
	sniffed := http.DetectContentType(head)
	for _, p := range []string{"image/", "audio/", "video/", "application/pdf"} {
		if strings.HasPrefix(sniffed, p) {
			return sniffed
		}
	}
	if byExt := mime.TypeByExtension(strings.ToLower(filepath.Ext(filename))); byExt != "" {
		return byExt
	}
	return sniffed
}

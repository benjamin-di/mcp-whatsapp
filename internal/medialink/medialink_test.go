package medialink

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var pdfBytes = []byte("%PDF-1.4\n%fake document body\n")

func newTestRegistry(t *testing.T) (*Registry, *time.Time) {
	t.Helper()
	r, err := New("https://example.com/whatsapp/")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	return r, &now
}

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// get requests link (its path only) from r and returns the recorder.
func get(t *testing.T, r *Registry, method, link string) *httptest.ResponseRecorder {
	t.Helper()
	path := strings.TrimPrefix(link, "https://example.com/whatsapp")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestNew_ValidatesPublicURL(t *testing.T) {
	for _, bad := range []string{"", "example.com", "ftp://example.com", "https://", "https://u:p@example.com",
		"https://example.com/x?a=1", "https://example.com/x#f"} {
		if _, err := New(bad); err == nil {
			t.Errorf("New(%q) accepted", bad)
		}
	}
	r, err := New("http://127.0.0.1:8765/")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	link, _, err := r.Issue("/tmp/x", "")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if !strings.HasPrefix(link, "http://127.0.0.1:8765/media/") || len(link) != len("http://127.0.0.1:8765/media/")+tokenLen {
		t.Errorf("link = %q", link)
	}
}

func TestLink_ServesFileOnce(t *testing.T) {
	r, _ := newTestRegistry(t)
	path := writeFile(t, "cache.bin", pdfBytes)
	link, expires, err := r.Issue(path, "Fee Agreement – signed.pdf")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if want := time.Date(2026, 10, 23, 12, 10, 0, 0, time.UTC); !expires.Equal(want) {
		t.Errorf("expires = %v, want %v", expires, want)
	}

	rec := get(t, r, http.MethodGet, link)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body)
	}
	if rec.Body.String() != string(pdfBytes) {
		t.Errorf("body = %q", rec.Body)
	}
	h := rec.Header()
	if h.Get("Content-Type") != "application/pdf" {
		t.Errorf("Content-Type = %q", h.Get("Content-Type"))
	}
	if h.Get("Content-Length") != "29" {
		t.Errorf("Content-Length = %q", h.Get("Content-Length"))
	}
	if cd := h.Get("Content-Disposition"); cd != "attachment; filename*=utf-8''Fee%20Agreement%20%E2%80%93%20signed.pdf" {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if h.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", h.Get("Cache-Control"))
	}

	if rec := get(t, r, http.MethodGet, link); rec.Code != http.StatusNotFound {
		t.Errorf("second GET = %d, want 404", rec.Code)
	}
}

func TestLink_HeadDoesNotSpend(t *testing.T) {
	r, _ := newTestRegistry(t)
	link, _, _ := r.Issue(writeFile(t, "a.pdf", pdfBytes), "")
	rec := get(t, r, http.MethodHead, link)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("HEAD = %d, body %d bytes", rec.Code, rec.Body.Len())
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename=a.pdf` {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if rec := get(t, r, http.MethodGet, link); rec.Code != http.StatusOK {
		t.Errorf("GET after HEAD = %d, want 200", rec.Code)
	}
}

func TestLink_Expires(t *testing.T) {
	r, now := newTestRegistry(t)
	link, _, _ := r.Issue(writeFile(t, "a.pdf", pdfBytes), "")
	*now = now.Add(TTL)
	if rec := get(t, r, http.MethodGet, link); rec.Code != http.StatusNotFound {
		t.Errorf("GET after TTL = %d, want 404", rec.Code)
	}
}

func TestLink_RejectsBadRequests(t *testing.T) {
	r, _ := newTestRegistry(t)
	link, _, _ := r.Issue(writeFile(t, "a.pdf", pdfBytes), "")

	if rec := get(t, r, http.MethodPost, link); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", rec.Code)
	}
	for _, path := range []string{"/media/", "/media/short", "/media/" + strings.Repeat("!", tokenLen),
		"/media/" + strings.Repeat("A", tokenLen)} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
	// None of the above spent the real link.
	if rec := get(t, r, http.MethodGet, link); rec.Code != http.StatusOK {
		t.Errorf("GET = %d, want 200", rec.Code)
	}
}

func TestLink_FileGone(t *testing.T) {
	r, _ := newTestRegistry(t)
	path := writeFile(t, "a.pdf", pdfBytes)
	link, _, _ := r.Issue(path, "")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if rec := get(t, r, http.MethodGet, link); rec.Code != http.StatusGone {
		t.Errorf("GET = %d, want 410", rec.Code)
	}
}

func TestIssue_BoundsOutstandingLinks(t *testing.T) {
	r, now := newTestRegistry(t)
	for i := 0; i < maxLinks; i++ {
		if _, _, err := r.Issue("/tmp/x", ""); err != nil {
			t.Fatalf("Issue %d: %v", i, err)
		}
	}
	if _, _, err := r.Issue("/tmp/x", ""); !errors.Is(err, ErrTooMany) {
		t.Fatalf("Issue over the cap: err = %v, want ErrTooMany", err)
	}
	*now = now.Add(TTL)
	if _, _, err := r.Issue("/tmp/x", ""); err != nil {
		t.Errorf("Issue after expiry: %v", err)
	}
}

func TestContentType(t *testing.T) {
	webp := []byte("RIFF\x00\x00\x00\x00WEBPVP8 ")
	cases := []struct {
		head     []byte
		filename string
		want     string
	}{
		{pdfBytes, "x.bin", "application/pdf"},
		{webp, "photo.jpg", "image/webp"},
		{[]byte(`{"a": 1}`), "data.json", "application/json"},
		{[]byte("hello"), "notes.unknownext", "text/plain; charset=utf-8"},
	}
	for _, c := range cases {
		if got := contentType(c.head, c.filename); got != c.want {
			t.Errorf("contentType(%q) = %q, want %q", c.filename, got, c.want)
		}
	}
}

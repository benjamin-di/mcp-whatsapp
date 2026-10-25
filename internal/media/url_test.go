package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type responseTransport func(*http.Request) (*http.Response, error)

func (f responseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPublicMediaAddresses(t *testing.T) {
	for _, value := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0", "224.0.0.1", "240.0.0.1", "192.0.2.1", "::1", "::ffff:127.0.0.1", "fc00::1", "fe80::1", "64:ff9b::7f00:1", "2002:7f00:1::1", "2001:db8::1"} {
		if publicIP(net.ParseIP(value)) {
			t.Errorf("accepted non-public address %s", value)
		}
	}
	for _, value := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "::ffff:8.8.8.8"} {
		if !publicIP(net.ParseIP(value)) {
			t.Errorf("refused public address %s", value)
		}
	}
}

func TestMediaDialPinsCheckedDNSAndRejectsMixedAnswers(t *testing.T) {
	lookups, dials := 0, 0
	lookup := func(context.Context, string) ([]net.IPAddr, error) {
		lookups++
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	dial := func(_ context.Context, _, address string) (net.Conn, error) {
		dials++
		if address != "8.8.8.8:443" {
			t.Fatalf("dialled unchecked address: %s", address)
		}
		return nil, errors.New("synthetic dial failure")
	}
	_, _ = publicDial(lookup, dial)(context.Background(), "tcp", "example.com:443")
	if lookups != 1 || dials != 1 {
		t.Fatalf("lookups=%d dials=%d", lookups, dials)
	}
	mixed := func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("127.0.0.1")}}, nil
	}
	_, err := publicDial(mixed, dial)(context.Background(), "tcp", "example.com:443")
	if err == nil || dials != 1 {
		t.Fatal("mixed DNS answer reached dial")
	}
}

func TestMediaURLPolicy(t *testing.T) {
	for _, raw := range []string{"http://example.com/file", "https://user:password@example.com/file", "https://example.com:8443/file", "file:///tmp/a", "https://example.com/file#fragment"} {
		u, err := url.Parse(raw)
		if err == nil && checkMediaURL(u) == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}

func TestURLRedirectPolicyRunsBeforeTheNextFetch(t *testing.T) {
	for _, target := range []string{"http://example.com/file", "https://user:secret@example.com/file", "https://example.com:8443/file"} {
		calls := 0
		client := mediaHTTPClient(responseTransport(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{target}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}))
		_, _, err := downloadURL(context.Background(), "https://example.com/single-use", t.TempDir(), client)
		if err == nil || calls != 1 {
			t.Fatalf("unsafe redirect followed: calls=%d err=%v", calls, err)
		}
	}
	calls := 0
	client := mediaHTTPClient(responseTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://example.com/loop"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	}))
	_, _, err := downloadURL(context.Background(), "https://example.com/loop", t.TempDir(), client)
	if err == nil || calls != 4 {
		t.Fatalf("redirect loop was not bounded: calls=%d err=%v", calls, err)
	}
}

func TestURLImageStagingAndCleanup(t *testing.T) {
	root := t.TempDir()
	body := append([]byte{0xff, 0xd8, 0xff}, bytes.Repeat([]byte{1}, 800)...)
	client := &http.Client{Transport: responseTransport(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" {
			t.Fatal("credentials attached")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}, nil
	})}
	path, cleanup, err := downloadURL(context.Background(), "https://example.com/downloads/opaque-token", root, client)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Ext(path) != ".jpg" {
		t.Fatalf("image extension was lost: %s", path)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatal("downloaded bytes differ")
	}
	stat, _ := os.Stat(path)
	if stat.Mode().Perm() != 0600 {
		t.Fatalf("file mode: %s", stat.Mode())
	}
	cleanup()
	files, _ := os.ReadDir(root)
	if len(files) != 0 {
		t.Fatal("temporary files were retained")
	}
}

func TestURLDownloadErrorsDoNotLeakTokensOrRetainFiles(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response *http.Response
		err      error
	}{
		{"network", nil, errors.New("https://example.com/secret-token")},
		{"http", &http.Response{StatusCode: 403, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("secret-token"))}, nil},
		{"html", &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("<!doctype html><html>login</html>")), ContentLength: -1}, nil},
		{"size", &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("x")), ContentLength: maxURLBytes + 1}, nil},
		{"truncated", &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("synthetic")), ContentLength: 900}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			client := &http.Client{Transport: responseTransport(func(*http.Request) (*http.Response, error) { return tc.response, tc.err })}
			_, _, err := downloadURL(context.Background(), "https://example.com/secret-token", root, client)
			if err == nil || strings.Contains(err.Error(), "secret-token") {
				t.Fatalf("unsafe error: %v", err)
			}
			files, _ := os.ReadDir(root)
			if len(files) != 0 {
				t.Fatal("failed download retained temporary files")
			}
		})
	}
}

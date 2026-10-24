package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbeAddr(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:8765":    "127.0.0.1:8765",
		"0.0.0.0:8765":      "127.0.0.1:8765",
		":8765":             "127.0.0.1:8765",
		"[::]:8765":         "[::1]:8765",
		"[::1]:8765":        "[::1]:8765",
		"192.168.1.10:9000": "192.168.1.10:9000",
		"localhost:8765":    "localhost:8765",
	}
	for in, want := range cases {
		if got := probeAddr(in); got != want {
			t.Errorf("probeAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRunHealthcheck(t *testing.T) {
	var gotPath string
	readyCode := http.StatusServiceUnavailable
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(http.StatusOK)
		case "/readyz":
			w.WriteHeader(readyCode)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	addr := srv.Listener.Addr().String()

	if code := runHealthcheck([]string{"-addr", addr}); code != 0 || gotPath != "/healthz" {
		t.Fatalf("healthz: exit %d path %q, want 0 /healthz", code, gotPath)
	}
	if code := runHealthcheck([]string{"-addr", addr, "-ready"}); code != 1 || gotPath != "/readyz" {
		t.Fatalf("readyz 503: exit %d path %q, want 1 /readyz", code, gotPath)
	}
	readyCode = http.StatusOK
	if code := runHealthcheck([]string{"-addr", addr, "-ready"}); code != 0 {
		t.Fatalf("readyz 200: exit %d, want 0", code)
	}

	// WHATSAPP_MCP_ADDR is honoured when -addr is absent, like serve.
	t.Setenv("WHATSAPP_MCP_ADDR", addr)
	if code := runHealthcheck(nil); code != 0 {
		t.Fatalf("env addr: exit %d, want 0", code)
	}
}

func TestRunHealthcheck_Unreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.Listener.Addr().String()
	srv.Close()
	if code := runHealthcheck([]string{"-addr", addr, "-timeout", "1s"}); code != 1 {
		t.Fatalf("exit %d, want 1 for a closed port", code)
	}
}

package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sealjay/mcp-whatsapp/internal/client"
)

// statusPairDriver is a fakePairDriver that also reports a connection
// status, like the production driver.
type statusPairDriver struct {
	*fakePairDriver
	mu sync.Mutex
	st client.ConnectionStatus
}

func (d *statusPairDriver) ConnectionStatus() client.ConnectionStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.st
}

func (d *statusPairDriver) set(st client.ConnectionStatus) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.st = st
}

// probeGet issues an optionally authenticated GET and returns the status
// code and decoded JSON body.
func probeGet(t *testing.T, url, token string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("%s Content-Type = %q, want application/json", url, ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("%s Cache-Control = %q, want no-store", url, cc)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode %s body %q: %v", url, raw, err)
	}
	return resp.StatusCode, body
}

// The probes answer without a token while /mcp and /pair still demand it.
func TestServer_HealthEndpointsUnauthenticated(t *testing.T) {
	drv := &statusPairDriver{fakePairDriver: newFakePairDriver(true)}
	since := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	drv.set(client.ConnectionStatus{
		State:       client.StateConnected,
		Ready:       true,
		Since:       since,
		LastEventAt: time.Now().Add(-30 * time.Second),
	})
	s, err := New(Config{Addr: "127.0.0.1:0", Driver: drv, AuthToken: "secret123"})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()
	<-s.listenerOK
	base := "http://" + s.BoundAddr()

	code, body := probeGet(t, base+"/healthz", "")
	if code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("/healthz = %d %v, want 200 status ok", code, body)
	}

	code, body = probeGet(t, base+"/readyz", "")
	if code != http.StatusOK {
		t.Fatalf("/readyz = %d %v, want 200", code, body)
	}
	if body["ready"] != true || body["state"] != "connected" || body["since"] != "2026-01-01T12:00:00Z" {
		t.Errorf("/readyz body = %v", body)
	}
	if age, ok := body["last_event_age_seconds"].(float64); !ok || age < 29 || age > 60 {
		t.Errorf("last_event_age_seconds = %v, want about 30", body["last_event_age_seconds"])
	}

	drv.set(client.ConnectionStatus{State: client.StateLoggedOut, Since: since, Reason: "401: logged out from another device"})
	code, body = probeGet(t, base+"/readyz", "")
	if code != http.StatusServiceUnavailable || body["ready"] != false || body["state"] != "logged_out" {
		t.Fatalf("/readyz logged out = %d %v, want 503 logged_out", code, body)
	}
	if _, ok := body["reason"]; ok {
		t.Errorf("/readyz must not expose reason: %v", body)
	}
	if _, ok := body["last_event_age_seconds"]; ok {
		t.Errorf("last_event_age_seconds should be absent before any event: %v", body)
	}

	// The token gate on /mcp is unchanged.
	resp, err := http.Get(base + "/mcp")
	if err != nil {
		t.Fatalf("GET /mcp: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/mcp without token = %d, want 401", resp.StatusCode)
	}

	// Only GET/HEAD are routed.
	resp, err = http.Post(base+"/healthz", "text/plain", strings.NewReader(""))
	if err != nil {
		t.Fatalf("POST /healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz = %d, want 405", resp.StatusCode)
	}
	resp, err = http.Head(base + "/readyz")
	if err != nil {
		t.Fatalf("HEAD /readyz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("HEAD /readyz = %d, want 503", resp.StatusCode)
	}
}

func TestReadyz_States(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		st    client.ConnectionStatus
		code  int
		state string
	}{
		{"connected", client.ConnectionStatus{State: client.StateConnected, Ready: true}, 200, "connected"},
		{"keepalive failing", client.ConnectionStatus{State: client.StateConnected, KeepAliveFailing: true}, 503, "connected"},
		{"connecting", client.ConnectionStatus{State: client.StateConnecting}, 503, "connecting"},
		{"disconnected", client.ConnectionStatus{State: client.StateDisconnected}, 503, "disconnected"},
		{"unpaired", client.ConnectionStatus{State: client.StateUnpaired}, 503, "unpaired"},
		{"stream replaced", client.ConnectionStatus{State: client.StateStreamReplaced}, 503, "stream_replaced"},
		{"client outdated", client.ConnectionStatus{State: client.StateClientOutdated}, 503, "client_outdated"},
		{"temporary ban", client.ConnectionStatus{State: client.StateTemporaryBan, BanCode: 101}, 503, "temporary_ban"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &statusPairDriver{st: tc.st}
			rec := httptest.NewRecorder()
			readyzHandler(src, func() time.Time { return now })(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if rec.Code != tc.code {
				t.Fatalf("code = %d, want %d", rec.Code, tc.code)
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body["state"] != tc.state || body["ready"] != (tc.code == 200) {
				t.Errorf("body = %v", body)
			}
			// Unauthenticated: only the coarse fields, never ban or reason detail.
			for k := range body {
				switch k {
				case "ready", "state", "since", "last_event_age_seconds":
				default:
					t.Errorf("unexpected /readyz field %q", k)
				}
			}
		})
	}
}

func TestReadyz_LastEventAge(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	src := &statusPairDriver{st: client.ConnectionStatus{
		State: client.StateConnected, Ready: true,
		Since:       now.Add(-time.Hour),
		LastEventAt: now.Add(-90*time.Second - 500*time.Millisecond),
	}}
	rec := httptest.NewRecorder()
	readyzHandler(src, func() time.Time { return now })(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	want := `{"ready":true,"state":"connected","since":"2026-01-01T11:00:00Z","last_event_age_seconds":90}` + "\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}

// A driver without ConnectionStatus (e.g. the plain test fake) reports
// unknown rather than guessing.
func TestReadyz_DriverWithoutStatus(t *testing.T) {
	s := newTestServer(t, newFakePairDriver(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()
	<-s.listenerOK

	code, body := probeGet(t, "http://"+s.BoundAddr()+"/readyz", "")
	if code != http.StatusServiceUnavailable || body["state"] != "unknown" || body["ready"] != false {
		t.Fatalf("/readyz = %d %v, want 503 unknown", code, body)
	}
}

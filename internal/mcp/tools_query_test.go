package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"go.mau.fi/whatsmeow/types"

	"github.com/sealjay/mcp-whatsapp/internal/client"
)

// fakePairCache is a minimal pairingCache for exercising pairing_status
// without standing up the daemon's real PairCache.
type fakePairCache struct {
	paired bool
	qr     string
}

func (f fakePairCache) Paired() bool { return f.paired }
func (f fakePairCache) QR() string   { return f.qr }

// callPairingStatus invokes the pairing_status handler and decodes its JSON
// "setup_state" envelope into a map.
func callPairingStatus(t *testing.T, s *Server) map[string]any {
	t.Helper()
	tool, ok := s.MCP().ListTools()["pairing_status"]
	if !ok || tool == nil || tool.Handler == nil {
		t.Fatal("pairing_status not registered")
	}
	res, err := tool.Handler(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatalf("pairing_status handler: %v", err)
	}
	if len(res.Content) == 0 {
		t.Fatal("pairing_status returned no content")
	}
	text, ok := mcp.AsTextContent(res.Content[0])
	if !ok {
		t.Fatalf("pairing_status content not text: %T", res.Content[0])
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(text.Text), &env); err != nil {
		t.Fatalf("decode envelope %q: %v", text.Text, err)
	}
	if env["type"] != "setup_state" {
		t.Errorf("type = %v, want setup_state", env["type"])
	}
	return env
}

// A nil pairing cache (smoke/tests) yields the error envelope without touching
// the WhatsApp client — so passing a nil client here must not panic.
func TestPairingStatus_NilCache(t *testing.T) {
	env := callPairingStatus(t, NewServer(nil, nil))
	if env["state"] != "error" {
		t.Errorf("state = %v, want error", env["state"])
	}
	if env["detail"] != "pairing cache unavailable" {
		t.Errorf("detail = %v, want pairing cache unavailable", env["detail"])
	}
}

// An unpaired cache with a pending QR yields awaiting_qr carrying the payload.
func TestPairingStatus_AwaitingQRWithPayload(t *testing.T) {
	env := callPairingStatus(t, NewServer(nil, fakePairCache{qr: "2@QRPAYLOAD"}))
	if env["state"] != "awaiting_qr" {
		t.Fatalf("state = %v, want awaiting_qr", env["state"])
	}
	if env["qr_payload"] != "2@QRPAYLOAD" {
		t.Errorf("qr_payload = %v, want 2@QRPAYLOAD", env["qr_payload"])
	}
	if env["detail"] != "Scan with WhatsApp → Linked devices" {
		t.Errorf("detail = %v, want the scan instruction", env["detail"])
	}
}

// An unpaired cache with no QR yet yields the generating-code envelope and
// omits qr_payload.
func TestPairingStatus_AwaitingQRGenerating(t *testing.T) {
	env := callPairingStatus(t, NewServer(nil, fakePairCache{}))
	if env["state"] != "awaiting_qr" {
		t.Fatalf("state = %v, want awaiting_qr", env["state"])
	}
	if _, ok := env["qr_payload"]; ok {
		t.Errorf("qr_payload should be absent, got %v", env["qr_payload"])
	}
	if env["detail"] != "Generating pairing code…" {
		t.Errorf("detail = %v, want generating message", env["detail"])
	}
}

// -- get_status ---------------------------------------------------------------

var statusNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// roundTrip decodes the payload the way an MCP client sees it.
func roundTrip(t *testing.T, v map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func TestStatusPayload_Connected(t *testing.T) {
	own := &types.JID{User: "447700000001", Server: types.DefaultUserServer}
	got := roundTrip(t, statusPayload(true, own, client.ConnectionStatus{
		State:         client.StateConnected,
		Ready:         true,
		Since:         statusNow.Add(-time.Hour),
		LastEventAt:   statusNow.Add(-5 * time.Second),
		LastMessageAt: statusNow.Add(-time.Minute),
	}, statusNow))

	want := map[string]any{
		"connected":              true,
		"paired":                 true,
		"state":                  "connected",
		"ready":                  true,
		"keepalive_failing":      false,
		"since":                  "2026-01-01T11:00:00Z",
		"last_event_at":          "2026-01-01T11:59:55Z",
		"last_event_age_seconds": float64(5),
		"last_message_at":        "2026-01-01T11:59:00Z",
		"own_jid":                "447700000001@s.whatsapp.net",
		"own_phone":              "447700000001",
	}
	if len(got) != len(want) {
		t.Errorf("fields = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

func TestStatusPayload_Unpaired(t *testing.T) {
	got := roundTrip(t, statusPayload(false, nil, client.ConnectionStatus{State: client.StateUnpaired, Since: statusNow}, statusNow))
	if got["paired"] != false || got["state"] != "unpaired" || got["ready"] != false {
		t.Fatalf("got %v", got)
	}
	if hint, _ := got["hint"].(string); !strings.Contains(hint, "/pair") {
		t.Errorf("hint = %q, want the pairing URL", hint)
	}
	for _, k := range []string{"own_jid", "own_phone", "last_event_at", "last_event_age_seconds", "last_message_at", "reason", "ban_code"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s should be absent: %v", k, got)
		}
	}
}

func TestStatusPayload_LoggedOut(t *testing.T) {
	got := roundTrip(t, statusPayload(false, nil, client.ConnectionStatus{
		State:  client.StateLoggedOut,
		Since:  statusNow,
		Reason: "401: logged out from another device",
	}, statusNow))
	if got["state"] != "logged_out" || got["reason"] != "401: logged out from another device" || got["paired"] != false {
		t.Fatalf("got %v", got)
	}
	if hint, _ := got["hint"].(string); !strings.Contains(hint, "Re-pair") {
		t.Errorf("hint = %q, want the re-pair hint", hint)
	}
}

func TestStatusPayload_TemporaryBan(t *testing.T) {
	own := &types.JID{User: "447700000001", Server: types.DefaultUserServer}
	got := roundTrip(t, statusPayload(false, own, client.ConnectionStatus{
		State:        client.StateTemporaryBan,
		Since:        statusNow,
		Reason:       "101: you sent too many messages to people who don't have you in their address books",
		BanCode:      101,
		BanExpiresAt: statusNow.Add(time.Hour),
	}, statusNow))
	if got["state"] != "temporary_ban" || got["ban_code"] != float64(101) || got["ban_expires_at"] != "2026-01-01T13:00:00Z" {
		t.Fatalf("got %v", got)
	}
	if _, ok := got["hint"]; !ok {
		t.Errorf("hint missing: %v", got)
	}
}

func TestStatusPayload_KeepAliveFailing(t *testing.T) {
	own := &types.JID{User: "447700000001", Server: types.DefaultUserServer}
	got := roundTrip(t, statusPayload(true, own, client.ConnectionStatus{
		State:            client.StateConnected,
		KeepAliveFailing: true,
	}, statusNow))
	if got["state"] != "connected" || got["ready"] != false || got["keepalive_failing"] != true {
		t.Fatalf("got %v", got)
	}
}

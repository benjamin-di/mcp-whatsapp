package client

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	wmstore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

var connT0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// at returns connT0 plus s seconds.
func at(s int) time.Time { return connT0.Add(time.Duration(s) * time.Second) }

func TestConnTracker_InitialState(t *testing.T) {
	var tr connTracker
	if got := tr.snapshot(false); got.State != StateUnpaired || got.Ready {
		t.Errorf("unpaired: got %+v, want unpaired and not ready", got)
	}
	if got := tr.snapshot(true); got.State != StateConnecting || got.Ready {
		t.Errorf("paired: got %+v, want connecting and not ready", got)
	}
}

func TestConnTracker_ConnectDisconnectReconnect(t *testing.T) {
	var tr connTracker

	tr.observe(&events.Connected{}, at(1))
	got := tr.snapshot(true)
	if got.State != StateConnected || !got.Ready || !got.Since.Equal(at(1)) {
		t.Fatalf("after Connected: %+v", got)
	}

	tr.observe(&events.Disconnected{}, at(5))
	got = tr.snapshot(true)
	if got.State != StateDisconnected || got.Ready || !got.Since.Equal(at(5)) || got.Reason == "" {
		t.Fatalf("after Disconnected: %+v", got)
	}

	tr.observe(&events.Connected{}, at(9))
	got = tr.snapshot(true)
	if got.State != StateConnected || !got.Ready || got.Reason != "" {
		t.Fatalf("after reconnect: %+v", got)
	}
	if !got.LastEventAt.Equal(at(9)) {
		t.Errorf("LastEventAt = %v, want %v", got.LastEventAt, at(9))
	}
}

func TestConnTracker_KeepAlive(t *testing.T) {
	var tr connTracker
	tr.observe(&events.Connected{}, at(1))

	tr.observe(&events.KeepAliveTimeout{ErrorCount: 1}, at(2))
	got := tr.snapshot(true)
	if got.State != StateConnected || !got.KeepAliveFailing || got.Ready {
		t.Fatalf("after KeepAliveTimeout: %+v, want connected, failing, not ready", got)
	}
	if !got.Since.Equal(at(1)) {
		t.Errorf("Since moved on keepalive timeout: %v", got.Since)
	}

	tr.observe(&events.KeepAliveRestored{}, at(3))
	if got := tr.snapshot(true); got.KeepAliveFailing || !got.Ready {
		t.Fatalf("after KeepAliveRestored: %+v, want ready", got)
	}

	// A reconnect also clears a stale keepalive failure.
	tr.observe(&events.KeepAliveTimeout{ErrorCount: 3}, at(4))
	tr.observe(&events.Connected{}, at(5))
	if got := tr.snapshot(true); !got.Ready {
		t.Fatalf("after reconnect: %+v, want ready", got)
	}
}

func TestConnTracker_TerminalStates(t *testing.T) {
	cases := []struct {
		name   string
		evt    any
		want   ConnState
		reason string
	}{
		{"logged out on connect", &events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, StateLoggedOut, "401: logged out from another device"},
		{"logged out without reason", &events.LoggedOut{}, StateLoggedOut, "logged out"},
		{"stream replaced", &events.StreamReplaced{}, StateStreamReplaced, "another client connected with this session"},
		{"client outdated", &events.ClientOutdated{}, StateClientOutdated, "client version rejected by WhatsApp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tr connTracker
			tr.observe(&events.Connected{}, at(1))
			tr.observe(tc.evt, at(2))
			// whatsmeow dispatches some of these from goroutines, so a
			// Disconnected may arrive afterwards; it must not mask the cause.
			tr.observe(&events.Disconnected{}, at(3))
			tr.observe(&events.ConnectFailure{Reason: events.ConnectFailureGeneric}, at(4))

			got := tr.snapshot(true)
			if got.State != tc.want || got.Ready {
				t.Fatalf("state = %q ready=%v, want %q not ready", got.State, got.Ready, tc.want)
			}
			if got.Reason != tc.reason {
				t.Errorf("reason = %q, want %q", got.Reason, tc.reason)
			}
			if !got.Since.Equal(at(2)) {
				t.Errorf("since = %v, want %v", got.Since, at(2))
			}
			// The terminal state survives the session being deleted.
			if got := tr.snapshot(false); got.State != tc.want {
				t.Errorf("unpaired snapshot state = %q, want %q", got.State, tc.want)
			}
		})
	}
}

func TestConnTracker_TemporaryBan(t *testing.T) {
	var tr connTracker
	tr.observe(&events.TemporaryBan{Code: events.TempBanSentToTooManyPeople, Expire: time.Hour}, at(10))
	got := tr.snapshot(true)
	if got.State != StateTemporaryBan || got.Ready {
		t.Fatalf("state = %+v", got)
	}
	if got.BanCode != 101 {
		t.Errorf("BanCode = %d, want 101", got.BanCode)
	}
	if want := at(10).Add(time.Hour); !got.BanExpiresAt.Equal(want) {
		t.Errorf("BanExpiresAt = %v, want %v", got.BanExpiresAt, want)
	}
	if got.Reason != events.TempBanSentToTooManyPeople.String() {
		t.Errorf("Reason = %q", got.Reason)
	}

	// Reconnecting after the ban clears the ban fields.
	tr.observe(&events.Connected{}, at(20))
	got = tr.snapshot(true)
	if got.State != StateConnected || got.BanCode != 0 || !got.BanExpiresAt.IsZero() {
		t.Fatalf("after reconnect: %+v", got)
	}
}

func TestConnTracker_ConnectFailure(t *testing.T) {
	var tr connTracker
	tr.observe(&events.ConnectFailure{Reason: events.ConnectFailureServiceUnavailable, Message: "ignored"}, at(1))
	got := tr.snapshot(true)
	if got.State != StateDisconnected || got.Reason != "connect failure: 503: unknown error" {
		t.Fatalf("got %+v", got)
	}
}

func TestConnTracker_PermanentDisconnectFallback(t *testing.T) {
	var tr connTracker
	tr.observe(&events.Connected{}, at(1))
	tr.observe(&events.CATRefreshError{}, at(2))
	if got := tr.snapshot(true); got.State != StateDisconnected || got.Reason != "CAT refresh failed" {
		t.Fatalf("got %+v", got)
	}
}

func TestConnTracker_PairSuccessLeavesLoggedOut(t *testing.T) {
	var tr connTracker
	tr.observe(&events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, at(1))
	tr.observe(&events.PairSuccess{}, at(2))
	got := tr.snapshot(true)
	if got.State != StateConnecting || got.Reason != "" || !got.Since.Equal(at(2)) {
		t.Fatalf("after PairSuccess: %+v", got)
	}
	tr.observe(&events.Connected{}, at(3))
	if got := tr.snapshot(true); !got.Ready {
		t.Fatalf("after Connected: %+v", got)
	}
}

// A manual logout (/pair/reset) deletes the session without an event; the
// snapshot must stop reporting connected.
func TestConnTracker_UnpairedOverridesNonTerminal(t *testing.T) {
	var tr connTracker
	tr.observe(&events.ConnectFailure{Reason: events.ConnectFailureGeneric}, at(1))
	tr.observe(&events.Connected{}, at(2))
	got := tr.snapshot(false)
	if got.State != StateUnpaired || got.Ready || got.Reason != "" {
		t.Fatalf("got %+v, want unpaired, not ready, no reason", got)
	}
}

func TestConnTracker_ActivityTimestamps(t *testing.T) {
	var tr connTracker
	tr.observe(&events.Connected{}, at(1))
	tr.observe(&events.Message{}, at(2))
	tr.observe(&events.Receipt{}, at(3))
	got := tr.snapshot(true)
	if !got.LastMessageAt.Equal(at(2)) {
		t.Errorf("LastMessageAt = %v, want %v", got.LastMessageAt, at(2))
	}
	if !got.LastEventAt.Equal(at(3)) {
		t.Errorf("LastEventAt = %v, want %v", got.LastEventAt, at(3))
	}
	if !got.Since.Equal(at(1)) {
		t.Errorf("Since = %v, want %v (activity must not move it)", got.Since, at(1))
	}

	tr.observe(&events.HistorySync{}, at(4))
	if got := tr.snapshot(true); !got.LastMessageAt.Equal(at(4)) {
		t.Errorf("LastMessageAt after HistorySync = %v, want %v", got.LastMessageAt, at(4))
	}
}

// Client.ConnectionStatus reads pairing from the device store.
func TestClient_ConnectionStatus(t *testing.T) {
	c := newUnpairedClient()
	if got := c.ConnectionStatus(); got.State != StateUnpaired {
		t.Fatalf("unpaired client: %+v", got)
	}

	device := &wmstore.Device{ID: &types.JID{User: "447700000001", Server: types.DefaultUserServer}}
	paired := &Client{wa: whatsmeow.NewClient(device, NewStderrLogger("test", "ERROR", false))}
	paired.conn.handleEvent(&events.Connected{})
	if got := paired.ConnectionStatus(); got.State != StateConnected || !got.Ready {
		t.Fatalf("paired client: %+v", got)
	}

	if got := newDisconnectedClient().ConnectionStatus(); got.State != StateUnpaired {
		t.Fatalf("client without whatsmeow: %+v", got)
	}
}

// The tracker is fed from whatsmeow's dispatcher and read from HTTP
// handlers concurrently; run under -race.
func TestConnTracker_Concurrent(t *testing.T) {
	var tr connTracker
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			tr.handleEvent(&events.Connected{})
			tr.handleEvent(&events.Disconnected{})
		}
	}()
	for i := 0; i < 200; i++ {
		_ = tr.snapshot(true)
	}
	<-done
}

package client

import (
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types/events"
)

// ConnState is the coarse WhatsApp connection state reported by get_status
// and the daemon's /readyz endpoint.
type ConnState string

const (
	// StateUnpaired: no stored session; the device must be linked via /pair or login.
	StateUnpaired ConnState = "unpaired"
	// StateConnecting: paired, but no Connected event yet (startup, or just after pairing).
	StateConnecting ConnState = "connecting"
	// StateConnected: the WebSocket is up and logged in.
	StateConnected ConnState = "connected"
	// StateDisconnected: the socket dropped or a connect attempt failed;
	// whatsmeow reconnects on its own for transient failures.
	StateDisconnected ConnState = "disconnected"
	// StateLoggedOut: WhatsApp revoked the session; re-pair to recover.
	StateLoggedOut ConnState = "logged_out"
	// StateStreamReplaced: another client connected with the same session keys.
	StateStreamReplaced ConnState = "stream_replaced"
	// StateClientOutdated: WhatsApp rejected this client version; upgrade.
	StateClientOutdated ConnState = "client_outdated"
	// StateTemporaryBan: WhatsApp temporarily banned the account.
	StateTemporaryBan ConnState = "temporary_ban"
)

// terminal reports whether whatsmeow will not recover from s on its own.
// Only Connected or PairSuccess leave a terminal state, so a late
// Disconnected (whatsmeow dispatches some events from goroutines) cannot
// mask why the connection ended.
func (s ConnState) terminal() bool {
	switch s {
	case StateLoggedOut, StateStreamReplaced, StateClientOutdated, StateTemporaryBan:
		return true
	}
	return false
}

// ConnectionStatus is a point-in-time snapshot of the connection state.
// It never contains JIDs, phone numbers or message content: Reason is one
// of whatsmeow's fixed code descriptions.
type ConnectionStatus struct {
	State ConnState
	// Ready is true only when paired, connected and keepalives are succeeding.
	Ready bool
	// Since is when State last changed (process start until the first change).
	Since time.Time
	// Reason explains disconnected and terminal states, e.g. "401: logged out from another device".
	Reason string
	// KeepAliveFailing is set between KeepAliveTimeout and KeepAliveRestored.
	KeepAliveFailing bool
	// LastEventAt is when any whatsmeow event was last received; zero if none yet.
	LastEventAt time.Time
	// LastMessageAt is when a live message or history sync was last received; zero if none yet.
	LastMessageAt time.Time
	// BanCode and BanExpiresAt describe a temporary ban (BanExpiresAt is zero when WhatsApp gave no expiry).
	BanCode      int
	BanExpiresAt time.Time
}

// connTracker folds whatsmeow events into a ConnectionStatus. The zero value
// is usable; New sets since to the client's creation time.
type connTracker struct {
	mu               sync.Mutex
	state            ConnState // "" until the first state-changing event
	since            time.Time
	reason           string
	keepAliveFailing bool
	lastEventAt      time.Time
	lastMessageAt    time.Time
	banCode          int
	banExpiresAt     time.Time
}

// handleEvent is registered with whatsmeow in New so it also sees pairing
// events, which arrive before StartEventHandler runs.
func (t *connTracker) handleEvent(evt any) { t.observe(evt, time.Now()) }

// observe records evt as received at now.
func (t *connTracker) observe(evt any, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastEventAt = now

	switch v := evt.(type) {
	case *events.Message, *events.HistorySync:
		t.lastMessageAt = now
	case *events.Connected:
		t.set(StateConnected, "", now)
	case *events.PairSuccess:
		// whatsmeow reconnects after pairing; Connected follows.
		t.set(StateConnecting, "", now)
	case *events.Disconnected:
		if !t.state.terminal() {
			t.set(StateDisconnected, "connection closed by server", now)
		}
	case *events.ConnectFailure:
		if !t.state.terminal() {
			t.set(StateDisconnected, "connect failure: "+v.Reason.String(), now)
		}
	case *events.KeepAliveTimeout:
		t.keepAliveFailing = true
	case *events.KeepAliveRestored:
		t.keepAliveFailing = false
	case *events.LoggedOut:
		reason := "logged out"
		if v.Reason != 0 {
			reason = v.Reason.String()
		}
		t.set(StateLoggedOut, reason, now)
	case *events.StreamReplaced:
		t.set(StateStreamReplaced, "another client connected with this session", now)
	case *events.ClientOutdated:
		t.set(StateClientOutdated, "client version rejected by WhatsApp", now)
	case *events.TemporaryBan:
		t.set(StateTemporaryBan, v.Code.String(), now)
		t.banCode = int(v.Code)
		if v.Expire > 0 {
			t.banExpiresAt = now.Add(v.Expire)
		}
	case events.PermanentDisconnect:
		// Remaining permanent disconnects (e.g. CATRefreshError): whatsmeow
		// will not reconnect, but a restart may recover.
		if !t.state.terminal() {
			t.set(StateDisconnected, v.PermanentDisconnectDescription(), now)
		}
	}
}

// set moves to state. Caller holds t.mu.
func (t *connTracker) set(state ConnState, reason string, now time.Time) {
	if t.state != state {
		t.since = now
	}
	t.state = state
	t.reason = reason
	t.keepAliveFailing = false
	t.banCode = 0
	t.banExpiresAt = time.Time{}
}

// snapshot reports the tracked state. paired is whether the device store
// currently holds a session: it decides the state before the first event,
// and a manual logout (which emits no event) reads as unpaired.
func (t *connTracker) snapshot(paired bool) ConnectionStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	state, reason := t.state, t.reason
	switch {
	case state == "" && paired:
		state = StateConnecting
	case !paired && !state.terminal():
		state, reason = StateUnpaired, ""
	}
	return ConnectionStatus{
		State:            state,
		Ready:            paired && state == StateConnected && !t.keepAliveFailing,
		Since:            t.since,
		Reason:           reason,
		KeepAliveFailing: t.keepAliveFailing,
		LastEventAt:      t.lastEventAt,
		LastMessageAt:    t.lastMessageAt,
		BanCode:          t.banCode,
		BanExpiresAt:     t.banExpiresAt,
	}
}

// ConnectionStatus reports the WhatsApp connection state tracked from
// whatsmeow events. Safe for concurrent use.
func (c *Client) ConnectionStatus() ConnectionStatus {
	paired := c.wa != nil && c.wa.Store != nil && c.wa.Store.ID != nil
	return c.conn.snapshot(paired)
}

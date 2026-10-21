package daemon

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/sealjay/mcp-whatsapp/internal/client"
)

// connStatuser is the optional driver surface /readyz reads. Production
// wiring satisfies it via *client.Client; drivers without it make /readyz
// report "unknown" (503).
type connStatuser interface {
	ConnectionStatus() client.ConnectionStatus
}

// readyzBody is the /readyz response. It is served without authentication
// (orchestrator probes rarely carry credentials), so it holds only the
// coarse state and timestamps: no JIDs, phone numbers, reasons or message
// data. get_status over authenticated /mcp has the detail.
type readyzBody struct {
	Ready bool   `json:"ready"`
	State string `json:"state"`
	// Since is when the state last changed (RFC 3339, UTC).
	Since string `json:"since,omitempty"`
	// LastEventAgeSeconds is the time since whatsmeow last delivered any
	// event; absent until the first one.
	LastEventAgeSeconds *int64 `json:"last_event_age_seconds,omitempty"`
}

// mountHealth registers the unauthenticated probe endpoints. requiresAuth
// does not match them, so they stay open when a bearer token gates /mcp
// and /pair.
func mountHealth(mux *http.ServeMux, drv pairDriver) {
	// GET patterns also match HEAD; other methods get 405.
	mux.HandleFunc("GET /healthz", handleHealthz)
	src, _ := drv.(connStatuser)
	mux.HandleFunc("GET /readyz", readyzHandler(src, time.Now))
}

// handleHealthz is the liveness probe: the HTTP server is serving. It does
// not depend on WhatsApp, so a disconnected or unpaired daemon (which still
// serves /pair and cached data) is not restarted by an orchestrator.
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeProbeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyzHandler is the readiness probe: 200 only when the device is paired,
// connected and its keepalives succeed; 503 otherwise.
func readyzHandler(src connStatuser, now func() time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if src == nil {
			writeProbeJSON(w, http.StatusServiceUnavailable, readyzBody{State: "unknown"})
			return
		}
		st := src.ConnectionStatus()
		body := readyzBody{Ready: st.Ready, State: string(st.State)}
		if !st.Since.IsZero() {
			body.Since = st.Since.UTC().Format(time.RFC3339)
		}
		if !st.LastEventAt.IsZero() {
			age := int64(now().Sub(st.LastEventAt) / time.Second)
			body.LastEventAgeSeconds = &age
		}
		code := http.StatusOK
		if !st.Ready {
			code = http.StatusServiceUnavailable
		}
		writeProbeJSON(w, code, body)
	}
}

func writeProbeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

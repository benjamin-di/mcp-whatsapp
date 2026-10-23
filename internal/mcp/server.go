// Package mcp wires mark3labs/mcp-go to the internal/client and internal/store
// packages, exposing the WhatsApp bridge over MCP Streamable HTTP.
package mcp

import (
	"net/http"

	"github.com/mark3labs/mcp-go/server"

	"github.com/sealjay/mcp-whatsapp/internal/client"
	"github.com/sealjay/mcp-whatsapp/internal/medialink"
)

// pairingCache is the minimal pairing-state surface pairing_status needs.
// *daemon.PairCache satisfies it. The smoke command and tests pass nil, in
// which case pairing_status reports an "error" envelope.
type pairingCache interface {
	Paired() bool
	QR() string
	Expired() bool
}

// Server holds the MCP server, its bound WhatsApp client, an optional
// pairing cache (used only by pairing_status) and optional download links
// (used only by download_media).
type Server struct {
	client   *client.Client
	mcp      *server.MCPServer
	cache    pairingCache
	readOnly bool
	links    *medialink.Registry
}

// Option configures NewServer.
type Option func(*Server)

// WithReadOnly registers only the tools readOnlyPolicies marks
// keepInReadOnly: nothing that sends, changes account or group state, or
// queries WhatsApp about third parties. get_status reports the mode.
func WithReadOnly(readOnly bool) Option {
	return func(s *Server) { s.readOnly = readOnly }
}

// Version is reported as serverInfo.version. cmd/whatsapp-mcp sets it from its
// -ldflags build version so the two cannot drift.
var Version = "dev"

// NewServer constructs an MCP server with all tools registered against the
// provided WhatsApp client. cache may be nil (smoke/tests); pairing_status is
// the only tool that consults it.
func NewServer(c *client.Client, cache pairingCache, opts ...Option) *Server {
	s := &Server{client: c, cache: cache}
	for _, opt := range opts {
		opt(s)
	}
	serverOpts := []server.ServerOption{server.WithToolCapabilities(true)}
	if s.readOnly {
		serverOpts = append(serverOpts, server.WithInstructions(readOnlyInstructions))
	}
	s.mcp = server.NewMCPServer("whatsapp", Version, serverOpts...)
	s.registerTools()
	s.registerResources()
	return s
}

// SetMediaLinks enables download_media's one-time download links and serves
// them at medialink.PathPrefix. Call before AttachHTTP.
func (s *Server) SetMediaLinks(r *medialink.Registry) { s.links = r }

// MCP returns the underlying mcp-go server (tests only).
func (s *Server) MCP() *server.MCPServer { return s.mcp }

// AttachHTTP mounts the MCP Streamable HTTP handler on mux at /mcp, and the
// download links at medialink.PathPrefix when enabled. The actual listener
// lifecycle is owned by the caller (internal/daemon).
func (s *Server) AttachHTTP(mux *http.ServeMux) {
	// Stateless: nothing here is session-scoped (the pairing cache is process-wide
	// device state), so legacy clients get no Mcp-Session-Id either.
	httpHandler := server.NewStreamableHTTPServer(s.mcp, server.WithStateLess(true))
	mux.Handle("/mcp", httpHandler)
	if s.links != nil {
		mux.Handle(medialink.PathPrefix, s.links)
	}
}

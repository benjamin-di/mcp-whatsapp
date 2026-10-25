package client

import (
	"context"
	"strings"
	"testing"

	"github.com/sealjay/mcp-whatsapp/internal/ratelimit"
)

func TestMediaURLIsNotConsumedWhenSendPreflightFails(t *testing.T) {
	c := newClientWithStore(t, newTestStoreWithLIDMap(t, nil))
	c.limiter = ratelimit.New(ratelimit.DefaultConfig())
	ctx := context.Background()
	const recipient = "447700000001@s.whatsapp.net"
	// If fetching runs before the preflight, this malformed URL would fail
	// instead of returning the connection/rate-limit failure.
	opts := SendMediaOptions{Recipient: recipient, SourceURL: "http://example.com/single-use"}
	if r := c.SendMediaWithOptions(ctx, opts); !strings.Contains(r.Message, "Not connected") {
		t.Fatalf("disconnected: %+v", r)
	}
	// Exhaust the same bucket used by an earlier send, without network I/O.
	if d := c.limiter.AllowSend(false); !d.Allowed {
		t.Fatal("initial send slot refused")
	}
	jid, err := parseRecipient(recipient)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.prepareSendMedia(ctx, jid, "", opts.SourceURL); err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("rate limit: %v", err)
	}
}

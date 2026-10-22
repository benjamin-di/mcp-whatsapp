package client

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/sealjay/mcp-whatsapp/internal/ratelimit"
)

// newOfflinePairedClient returns a Client with a message store, the given
// limiter and a whatsmeow client whose paired device lives in a throwaway
// sqlstore. It never connects, so tests drive sendReply and sendReaction
// with a fake send function.
func newOfflinePairedClient(t *testing.T, limiter *ratelimit.Limiter) *Client {
	t.Helper()
	container, err := sqlstore.New(context.Background(), "sqlite3",
		"file:"+filepath.Join(t.TempDir(), "whatsapp.db")+"?_foreign_keys=on", waLog.Noop)
	if err != nil {
		t.Fatalf("sqlstore.New: %v", err)
	}
	t.Cleanup(func() { _ = container.Close() })
	device := container.NewDevice()
	device.ID = &types.JID{User: "447700000010", Device: 3, Server: types.DefaultUserServer}
	// Wire the contact and LID stores the way sqlstore does for a paired
	// device, without saving one.
	device.SetAllStores(sqlstore.NewSQLStore(container, *device.ID))
	device.LIDs = container.LIDMap
	device.Initialized = true

	c := newClientWithStore(t, newTestStoreWithLIDMap(t, nil))
	c.wa = whatsmeow.NewClient(device, NewStderrLogger("test", "ERROR", false))
	c.limiter = limiter
	return c
}

type sentMessage struct {
	to  types.JID
	msg *waProto.Message
}

// recordSends returns a sendFunc that records every message and answers with
// the IDs SENT-1, SENT-2, ...
func recordSends(sent *[]sentMessage) sendFunc {
	return func(_ context.Context, to types.JID, msg *waProto.Message, _ ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		*sent = append(*sent, sentMessage{to: to, msg: msg})
		return whatsmeow.SendResponse{ID: types.MessageID(fmt.Sprintf("SENT-%d", len(*sent)))}, nil
	}
}

func TestSendReply_GoesThroughRateLimiter(t *testing.T) {
	c := newOfflinePairedClient(t, ratelimit.New(ratelimit.DefaultConfig()))
	var sent []sentMessage
	ctx := context.Background()
	const chat = "447700000001@s.whatsapp.net"

	if err := c.sendReply(ctx, recordSends(&sent), chat, "a1", "", "first"); err != nil {
		t.Fatalf("first reply: %v", err)
	}
	err := c.sendReply(ctx, recordSends(&sent), chat, "a1", "", "second")
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("second reply within the send interval: err = %v, want rate limited", err)
	}
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sent))
	}

	// The operator override (X-Rate-Limit-Override) still bypasses it.
	if err := c.sendReply(ratelimit.WithBypass(ctx), recordSends(&sent), chat, "a1", "", "third"); err != nil {
		t.Fatalf("reply with the override: %v", err)
	}
}

func TestSendReply_StoresSentReply(t *testing.T) {
	c := newOfflinePairedClient(t, nil)
	var sent []sentMessage
	ctx := context.Background()

	if err := c.sendReply(ctx, recordSends(&sent), "447700000001@s.whatsapp.net", "a1", "", "see you then"); err != nil {
		t.Fatalf("sendReply: %v", err)
	}
	got, err := c.store.GetMessageContext(ctx, "SENT-1", 1, 1)
	if err != nil {
		t.Fatalf("sent reply not cached: %v", err)
	}
	m := got.Message
	if m.ChatJID != "447700000001@s.whatsapp.net" || m.Content != "see you then" || !m.IsFromMe {
		t.Errorf("cached reply = %+v, want chat 447700000001@s.whatsapp.net, content %q, from me", m, "see you then")
	}
}

func TestSendReply_AcceptsBarePhoneDigits(t *testing.T) {
	c := newOfflinePairedClient(t, nil)
	var sent []sentMessage

	if err := c.sendReply(context.Background(), recordSends(&sent), "447700000001", "a1", "+447700000002", "ok"); err != nil {
		t.Fatalf("sendReply: %v", err)
	}
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sent))
	}
	if got := sent[0].to.String(); got != "447700000001@s.whatsapp.net" {
		t.Errorf("sent to %s, want 447700000001@s.whatsapp.net", got)
	}
	ci := sent[0].msg.GetExtendedTextMessage().GetContextInfo()
	if ci.GetStanzaID() != "a1" || ci.GetParticipant() != "447700000002@s.whatsapp.net" {
		t.Errorf("quote = {StanzaID %q, Participant %q}, want {a1, 447700000002@s.whatsapp.net}", ci.GetStanzaID(), ci.GetParticipant())
	}
	if got := sent[0].msg.GetExtendedTextMessage().GetText(); got != "ok" {
		t.Errorf("text = %q, want %q", got, "ok")
	}
}

// A malformed chat is rejected before the limiter, so it uses up no budget.
func TestSendReply_RejectsMalformedChat(t *testing.T) {
	c := newOfflinePairedClient(t, ratelimit.New(ratelimit.DefaultConfig()))
	var sent []sentMessage
	ctx := context.Background()

	err := c.sendReply(ctx, recordSends(&sent), "447 700 000001", "a1", "", "hi")
	if err == nil || !strings.Contains(err.Error(), "invalid chat JID") {
		t.Fatalf("err = %v, want invalid chat JID", err)
	}
	if err := c.sendReply(ctx, recordSends(&sent), "447700000001", "a1", "", "hi"); err != nil {
		t.Fatalf("valid reply after a rejected one: %v", err)
	}
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sent))
	}
}

func TestSendReaction_GoesThroughRateLimiter(t *testing.T) {
	c := newOfflinePairedClient(t, ratelimit.New(ratelimit.DefaultConfig()))
	var sent []sentMessage
	ctx := context.Background()
	const chat = "447700000001@s.whatsapp.net"

	if err := c.sendReaction(ctx, recordSends(&sent), chat, "a1", "", "👍"); err != nil {
		t.Fatalf("first reaction: %v", err)
	}
	err := c.sendReaction(ctx, recordSends(&sent), chat, "a3", "", "😂")
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("second reaction within the send interval: err = %v, want rate limited", err)
	}
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sent))
	}
	if err := c.sendReaction(ratelimit.WithBypass(ctx), recordSends(&sent), chat, "a3", "", "😂"); err != nil {
		t.Fatalf("reaction with the override: %v", err)
	}
}

func TestSendReaction_AcceptsBarePhoneDigits(t *testing.T) {
	c := newOfflinePairedClient(t, nil)
	var sent []sentMessage
	ctx := context.Background()

	if err := c.sendReaction(ctx, recordSends(&sent), "447700000001", "a1", "", "👍"); err != nil {
		t.Fatalf("direct-chat reaction: %v", err)
	}
	if err := c.sendReaction(ctx, recordSends(&sent), "120363025246125486@g.us", "g1", "447700000002", "👍"); err != nil {
		t.Fatalf("group reaction: %v", err)
	}
	if len(sent) != 2 {
		t.Fatalf("sent %d messages, want 2", len(sent))
	}

	direct := sent[0].msg.GetReactionMessage().GetKey()
	if sent[0].to.String() != "447700000001@s.whatsapp.net" || direct.GetRemoteJID() != "447700000001@s.whatsapp.net" {
		t.Errorf("direct reaction to %s with key chat %q, want 447700000001@s.whatsapp.net", sent[0].to, direct.GetRemoteJID())
	}
	group := sent[1].msg.GetReactionMessage().GetKey()
	if group.GetParticipant() != "447700000002@s.whatsapp.net" || group.GetID() != "g1" {
		t.Errorf("group reaction key = {Participant %q, ID %q}, want {447700000002@s.whatsapp.net, g1}", group.GetParticipant(), group.GetID())
	}
}

func TestSendReplyAndReaction_NotConnected(t *testing.T) {
	c := newDisconnectedClient()
	assertNotConnected(t, c.SendReply(context.Background(), "447700000001", "a1", "", "hi"))
	assertNotConnected(t, c.SendReaction(context.Background(), "447700000001", "a1", "", "👍"))
}

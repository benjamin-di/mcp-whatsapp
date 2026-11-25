package client

import (
	"context"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
)

// TestParseRecipient covers the recipient-string → types.JID conversion used
// by every send tool. The headline behaviour: a leading `+` on a bare phone
// number is stripped (so E.164-with-plus is accepted as a courtesy), and any
// other non-digit input on the bare-number path is rejected with a clear
// error instead of silently constructing an invalid JID. Fully-qualified JIDs
// containing `@` are delegated to whatsmeow's own parser unchanged.
func TestParseRecipient(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantUser   string
		wantServer string
		wantErr    string // substring expected in error message when set
	}{
		{
			name:       "bare digits",
			in:         "447967960994",
			wantUser:   "447967960994",
			wantServer: types.DefaultUserServer,
		},
		{
			name:       "leading plus is stripped",
			in:         "+447967960994",
			wantUser:   "447967960994",
			wantServer: types.DefaultUserServer,
		},
		{
			name:    "empty rejected",
			in:      "",
			wantErr: "recipient",
		},
		{
			name:    "bare plus rejected",
			in:      "+",
			wantErr: "recipient",
		},
		{
			name:    "spaces rejected",
			in:      "447 967 960994",
			wantErr: "recipient",
		},
		{
			name:    "punctuation rejected",
			in:      "447-967-960994",
			wantErr: "recipient",
		},
		{
			name:    "plus with spaces rejected",
			in:      "+44 7967 960994",
			wantErr: "recipient",
		},
		{
			name:    "non-digit rejected",
			in:      "abc",
			wantErr: "recipient",
		},
		{
			name:       "fully qualified user JID passes through",
			in:         "447967960994@s.whatsapp.net",
			wantUser:   "447967960994",
			wantServer: types.DefaultUserServer,
		},
		{
			name:       "group JID passes through",
			in:         "120363025246125486@g.us",
			wantUser:   "120363025246125486",
			wantServer: types.GroupServer,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRecipient(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("parseRecipient(%q): want error containing %q, got JID %+v", tc.in, tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseRecipient(%q): error = %q, want substring %q", tc.in, err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRecipient(%q): unexpected error %v", tc.in, err)
			}
			if got.User != tc.wantUser {
				t.Errorf("parseRecipient(%q): User = %q, want %q", tc.in, got.User, tc.wantUser)
			}
			if got.Server != tc.wantServer {
				t.Errorf("parseRecipient(%q): Server = %q, want %q", tc.in, got.Server, tc.wantServer)
			}
		})
	}
}

func TestMediaTypeFromExtDocuments(t *testing.T) {
	cases := map[string]string{
		"/x/a.pdf":  "application/pdf",
		"/x/a.PDF":  "application/pdf",
		"/x/a.docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"/x/a.xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"/x/a.csv":  "text/csv",
		"/x/a.zip":  "application/zip",
		"/x/noext":  "application/octet-stream",
		"/x/a.zzzz": "application/octet-stream",
	}
	for path, want := range cases {
		mt, got := mediaTypeFromExt(path)
		if mt != whatsmeow.MediaDocument {
			t.Errorf("%s: media type = %v, want document", path, mt)
		}
		if got != want {
			t.Errorf("%s: mime = %q, want %q", path, got, want)
		}
	}
}

func TestSendWithID_PassesExtraIDAndPersists(t *testing.T) {
	c, _ := newOfflinePairedClient(t, nil)
	var gotExtra []whatsmeow.SendRequestExtra
	c.waSend = func(_ context.Context, _ types.JID, _ *waProto.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		gotExtra = append([]whatsmeow.SendRequestExtra(nil), extra...)
		id := types.MessageID("AUTO-1")
		if len(extra) > 0 && extra[0].ID != "" {
			id = extra[0].ID
		}
		return whatsmeow.SendResponse{ID: id}, nil
	}

	ctx := context.Background()
	const clientID = "3EB0ABCDEF0123456789ABCDEF012345"
	res := c.SendWithID(ctx, "447700000001@s.whatsapp.net", "hello with id", clientID)
	if !res.Success {
		t.Fatalf("SendWithID failed: %s", res.Message)
	}
	if res.ID != clientID {
		t.Fatalf("res.ID = %q, want %q", res.ID, clientID)
	}
	if len(gotExtra) != 1 || gotExtra[0].ID != types.MessageID(clientID) {
		t.Fatalf("gotExtra = %+v, want single SendRequestExtra with ID %q", gotExtra, clientID)
	}
	if _, err := c.store.GetMessageContext(ctx, clientID, 1, 1); err != nil {
		t.Fatalf("sent message with client ID not persisted in store: %v", err)
	}

	// Empty client_message_id preserves default behavior (no SendRequestExtra).
	gotExtra = nil
	res2 := c.SendWithID(ctx, "447700000001@s.whatsapp.net", "hello default", "")
	if !res2.Success || res2.ID != "AUTO-1" {
		t.Fatalf("SendWithID empty ID: got %+v, want Success with AUTO-1", res2)
	}
	if len(gotExtra) != 0 {
		t.Fatalf("empty client_message_id passed unexpected extra: %+v", gotExtra)
	}
}

func TestSendWithID_RejectsInvalidClientMessageID(t *testing.T) {
	c, _ := newOfflinePairedClient(t, nil)
	called := false
	c.waSend = func(_ context.Context, _ types.JID, _ *waProto.Message, _ ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		called = true
		return whatsmeow.SendResponse{ID: "SHOULD-NOT-SEND"}, nil
	}
	for _, bad := range []string{"bad id", "bad\nid", "bad]id", strings.Repeat("A", 65)} {
		res := c.SendWithID(context.Background(), "447700000001@s.whatsapp.net", "hello", bad)
		if res.Success {
			t.Errorf("SendWithID(%q) succeeded, want failure", bad)
		}
		if called {
			t.Errorf("SendWithID(%q) called waSend despite invalid ID", bad)
		}
	}
}

func TestSendWithID_RefusesReusedAndInFlightID(t *testing.T) {
	c, _ := newOfflinePairedClient(t, nil)
	sendCount := 0
	c.waSend = func(_ context.Context, _ types.JID, _ *waProto.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		sendCount++
		return whatsmeow.SendResponse{ID: extra[0].ID}, nil
	}

	ctx := context.Background()
	const clientID = "3EB0REUSE0123456789ABCDEF0123456"
	first := c.SendWithID(ctx, "447700000001@s.whatsapp.net", "original body", clientID)
	if !first.Success {
		t.Fatalf("first SendWithID failed: %s", first.Message)
	}

	// Reusing the same client_message_id must fail without calling waSend or overwriting history.
	dup := c.SendWithID(ctx, "447700000002@s.whatsapp.net", "conflicting body", clientID)
	if dup.Success {
		t.Fatalf("duplicate SendWithID succeeded, want refusal")
	}
	if sendCount != 1 {
		t.Fatalf("sendCount = %d, want 1", sendCount)
	}
	mc, err := c.store.GetMessageContext(ctx, clientID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessageContext: %v", err)
	}
	if mc.Message.Content != "original body" {
		t.Fatalf("stored content = %q, want %q", mc.Message.Content, "original body")
	}

	// An in-flight client_message_id must be refused before calling waSend.
	const inflightID = "3EB0INFLIGHT1234567890ABCDEF0123"
	c.clientMsgIDMu.Lock()
	c.inFlightClientIDs = map[string]struct{}{inflightID: {}}
	c.clientMsgIDMu.Unlock()
	res := c.SendWithID(ctx, "447700000001@s.whatsapp.net", "concurrent body", inflightID)
	if res.Success || !strings.Contains(res.Message, "in flight") {
		t.Fatalf("in-flight SendWithID = %+v, want in-flight refusal", res)
	}
	if sendCount != 1 {
		t.Fatalf("sendCount after in-flight refusal = %d, want 1", sendCount)
	}
}

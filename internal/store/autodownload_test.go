package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestAutoDownloadFailure_RoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const chatJID = "447700000001@s.whatsapp.net"

	got, err := s.HasAutoDownloadFailure(ctx, "media-1", chatJID)
	if err != nil {
		t.Fatalf("HasAutoDownloadFailure: %v", err)
	}
	if got {
		t.Fatal("fresh store reports a failure")
	}

	if err := s.RecordAutoDownloadFailure(ctx, "media-1", chatJID, "expired (HTTP 410)", time.Now()); err != nil {
		t.Fatalf("RecordAutoDownloadFailure: %v", err)
	}
	// Recording twice is an upsert, not a constraint error.
	if err := s.RecordAutoDownloadFailure(ctx, "media-1", chatJID, "expired (HTTP 404)", time.Now()); err != nil {
		t.Fatalf("second RecordAutoDownloadFailure: %v", err)
	}
	if got, err := s.HasAutoDownloadFailure(ctx, "media-1", chatJID); err != nil || !got {
		t.Fatalf("HasAutoDownloadFailure after record = %v, %v; want true", got, err)
	}
	// Keyed by (message_id, chat_jid): the same ID in another chat is distinct.
	if got, err := s.HasAutoDownloadFailure(ctx, "media-1", "123456789@g.us"); err != nil || got {
		t.Fatalf("HasAutoDownloadFailure other chat = %v, %v; want false", got, err)
	}

	// Re-delivering the message (INSERT OR REPLACE) must not forget the failure.
	if err := s.StoreMessage(ctx, Message{
		ID: "media-1", ChatJID: chatJID, Sender: "447700000001", Timestamp: time.Now(),
		MediaType: "image", Filename: "image_x.jpg",
	}, nil, nil, nil, 1); err != nil {
		t.Fatalf("StoreMessage: %v", err)
	}
	if got, err := s.HasAutoDownloadFailure(ctx, "media-1", chatJID); err != nil || !got {
		t.Fatalf("failure lost after message re-store: %v, %v", got, err)
	}
}

// TestOpen_AddsAutoDownloadFailuresTable: a database created before the
// table existed gets it on Open, keeping existing rows.
func TestOpen_AddsAutoDownloadFailuresTable(t *testing.T) {
	dir := t.TempDir()
	legacy, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, "messages.db")+"?_foreign_keys=on")
	if err != nil {
		t.Fatalf("open legacy DB: %v", err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
		CREATE TABLE messages (
			id TEXT, chat_jid TEXT, sender TEXT, content TEXT,
			timestamp TIMESTAMP, is_from_me BOOLEAN,
			media_type TEXT, filename TEXT, url TEXT, direct_path TEXT,
			media_key BLOB, file_sha256 BLOB, file_enc_sha256 BLOB,
			file_length INTEGER, poll_options_json TEXT,
			PRIMARY KEY (id, chat_jid),
			FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		);
		INSERT INTO chats (jid, name) VALUES ('legacy@s.whatsapp.net', 'Legacy');
		INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
		VALUES ('legacy-1', 'legacy@s.whatsapp.net', 'legacy', 'x', '2026-01-01 09:00:00', 0);
	`); err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}
	_ = legacy.Close()

	for i := range 2 { // second Open proves the upgrade is idempotent
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("Open #%d: %v", i+1, err)
		}
		if err := s.RecordAutoDownloadFailure(context.Background(), "legacy-1", "legacy@s.whatsapp.net", "expired (HTTP 404)", time.Now()); err != nil {
			t.Fatalf("Open #%d: record failure: %v", i+1, err)
		}
		var n int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&n); err != nil || n != 1 {
			t.Fatalf("Open #%d: legacy rows = %d, %v; want 1", i+1, n, err)
		}
		s.Close()
	}
}

func TestListAutoDownloadCandidates(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const alice, team = "447700000001@s.whatsapp.net", "123456789@g.us"
	day := func(d int) time.Time { return time.Date(2026, 2, d, 10, 0, 0, 0, time.UTC) }
	media := func(id, chat, mediaType string, ts time.Time, size uint64) {
		t.Helper()
		if err := s.StoreMessage(ctx, Message{
			ID: id, ChatJID: chat, Sender: "447700000001", Timestamp: ts,
			MediaType: mediaType, Filename: id + ".bin",
		}, []byte{1}, []byte{2}, []byte{3}, size); err != nil {
			t.Fatalf("StoreMessage %s: %v", id, err)
		}
	}
	media("img-old", alice, "image", day(1), 100)
	media("doc-mid", team, "document", day(5), 100)
	media("img-tie-a", alice, "image", day(10), 100) // same timestamp as the next two
	media("img-tie-b", alice, "image", day(10), 100)
	media("img-tie-b", team, "image", day(10), 100) // same ID, other chat
	media("img-new", team, "image", day(20), 100)
	media("vid", alice, "video", day(21), 100)        // type not selected
	media("img-big", alice, "image", day(22), 10_000) // over the cap
	media("img-nosize", alice, "image", day(23), 0)   // Download refuses it
	media("img-failed", alice, "image", day(24), 100) // permanent failure recorded
	if err := s.RecordAutoDownloadFailure(ctx, "img-failed", alice, "expired (HTTP 410)", time.Now()); err != nil {
		t.Fatal(err)
	}

	type key struct{ id, chat string }
	list := func(q AutoDownloadCandidateQuery) []key {
		t.Helper()
		var out []key
		for {
			page, err := s.ListAutoDownloadCandidates(ctx, q)
			if err != nil {
				t.Fatalf("ListAutoDownloadCandidates: %v", err)
			}
			for _, c := range page {
				out = append(out, key{c.MessageID, c.ChatJID})
			}
			if len(page) < q.Limit || len(page) == 0 {
				return out
			}
			q.After = page[len(page)-1]
		}
	}
	base := AutoDownloadCandidateQuery{Types: []string{"image", "document"}, MaxBytes: 5000}

	want := []key{
		{"img-new", team},
		{"img-tie-b", alice}, {"img-tie-b", team}, {"img-tie-a", alice}, // ties: id DESC, chat DESC
		{"doc-mid", team},
		{"img-old", alice},
		{"a4", alice}, // seed fixture, 2026-01-04, 1234 bytes
	}
	for _, limit := range []int{1, 2, 3, 100} { // paging must neither skip nor repeat
		q := base
		q.Limit = limit
		if got := list(q); !reflect.DeepEqual(got, want) {
			t.Fatalf("limit %d:\n got %v\nwant %v", limit, got, want)
		}
	}

	q := base
	q.Limit = 100
	q.Since = day(5)
	if got := list(q); !reflect.DeepEqual(got, want[:5]) {
		t.Fatalf("since day 5: got %v, want %v", got, want[:5])
	}

	q = AutoDownloadCandidateQuery{Types: []string{"image"}, Limit: 100} // no size cap
	got := list(q)
	if len(got) != 7 || got[0] != (key{"img-big", alice}) {
		t.Fatalf("no cap: got %v, want img-big first and 7 rows", got)
	}

	if page, err := s.ListAutoDownloadCandidates(ctx, AutoDownloadCandidateQuery{}); err != nil || page != nil {
		t.Fatalf("no types: %v, %v; want nothing", page, err)
	}
}

// A message deleted for everyone is not a backfill candidate unless the
// server keeps revoked content. Revoking without keeping content clears the
// media fields, so such a row never qualifies; a row revoked by an earlier
// -keep-revoked run still has them and is excluded by revoked_at alone.
func TestListAutoDownloadCandidates_Revoked(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const alice = "447700000001@s.whatsapp.net"
	at := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	for _, id := range []string{"kept", "revoked-kept", "revoked-cleared"} {
		if err := s.StoreMessage(ctx, Message{
			ID: id, ChatJID: alice, Sender: "447700000001", Timestamp: at,
			MediaType: "image", Filename: id + ".jpg",
		}, []byte{1}, []byte{2}, []byte{3}, 100); err != nil {
			t.Fatalf("StoreMessage %s: %v", id, err)
		}
	}
	author := Author{Senders: []string{"447700000001"}}
	if ok, err := s.ApplyRevoke(ctx, alice, "revoked-kept", author, at, true); err != nil || !ok {
		t.Fatalf("ApplyRevoke(keep) = %v, %v", ok, err)
	}
	if ok, err := s.ApplyRevoke(ctx, alice, "revoked-cleared", author, at, false); err != nil || !ok {
		t.Fatalf("ApplyRevoke(clear) = %v, %v", ok, err)
	}

	ids := func(includeRevoked bool) []string {
		t.Helper()
		page, err := s.ListAutoDownloadCandidates(ctx, AutoDownloadCandidateQuery{
			Types: []string{"image"}, Since: at, Limit: 100, IncludeRevoked: includeRevoked,
		})
		if err != nil {
			t.Fatalf("ListAutoDownloadCandidates: %v", err)
		}
		var out []string
		for _, c := range page {
			out = append(out, c.MessageID)
		}
		return out
	}
	if got, want := ids(false), []string{"kept"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("default: got %v, want %v (revoked rows excluded)", got, want)
	}
	if got, want := ids(true), []string{"revoked-kept", "kept"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("IncludeRevoked: got %v, want %v (only the row that still has its media)", got, want)
	}
}

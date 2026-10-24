package store

import (
	"context"
	"database/sql"
	"path/filepath"
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

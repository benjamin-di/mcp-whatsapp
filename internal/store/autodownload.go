package store

import (
	"context"
	"time"
)

// RecordAutoDownloadFailure remembers that automatic media download gave up
// on a message for good (typically because WhatsApp's CDN no longer has the
// file). The row lives in its own table rather than on messages so that a
// later StoreMessage (INSERT OR REPLACE on re-delivered history) cannot
// erase it. It only gates automatic attempts; download_media still tries.
func (s *Store) RecordAutoDownloadFailure(ctx context.Context, messageID, chatJID, reason string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO media_auto_download_failures (message_id, chat_jid, reason, failed_at)
		VALUES (?, ?, ?, ?)`,
		messageID, chatJID, reason, at,
	)
	return err
}

// HasAutoDownloadFailure reports whether RecordAutoDownloadFailure has been
// called for the message.
func (s *Store) HasAutoDownloadFailure(ctx context.Context, messageID, chatJID string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM media_auto_download_failures WHERE message_id = ? AND chat_jid = ?)",
		messageID, chatJID,
	).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists == 1, nil
}

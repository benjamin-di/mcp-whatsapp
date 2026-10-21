package store

import (
	"context"
	"strings"
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

// AutoDownloadCandidate is a stored media message that automatic download
// may still need to fetch: one of the requested types, within the size cap
// and without a recorded permanent failure. Whether its file is already in
// the media cache is for the caller to check.
type AutoDownloadCandidate struct {
	MessageID  string
	ChatJID    string
	MediaType  string
	Filename   string
	FileLength uint64

	// ts is the row's timestamp exactly as stored, so the next page can
	// resume after this row without a time-format round trip.
	ts string
}

// AutoDownloadCandidateQuery selects one page of candidates. Zero values
// mean "unset" except for Limit, which defaults to 256 when <= 0.
type AutoDownloadCandidateQuery struct {
	Types    []string  // media types to include; empty returns nothing
	MaxBytes uint64    // skip files declared larger than this; 0 = no limit
	Since    time.Time // skip messages older than this
	// After resumes the newest-first scan after this candidate (the last
	// one of the previous page). The zero value starts at the newest.
	After AutoDownloadCandidate
	Limit int
}

// autoDownloadCandidateFilters are the conditions every candidate meets,
// whatever the query. A new exclusion (for example messages that were
// deleted for everyone) belongs here.
var autoDownloadCandidateFilters = []string{
	"m.file_length > 0",
	`NOT EXISTS (SELECT 1 FROM media_auto_download_failures f
		WHERE f.message_id = m.id AND f.chat_jid = m.chat_jid)`,
}

// ListAutoDownloadCandidates returns one page of media messages for the
// auto-download backfill, newest first. Rows are ordered by (timestamp, id,
// chat_jid) so paging with After neither skips nor repeats a row, even when
// many messages share a timestamp.
func (s *Store) ListAutoDownloadCandidates(ctx context.Context, q AutoDownloadCandidateQuery) ([]AutoDownloadCandidate, error) {
	if len(q.Types) == 0 {
		return nil, nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 256
	}

	where := append([]string{}, autoDownloadCandidateFilters...)
	var args []any
	where = append(where, "m.media_type IN (?"+strings.Repeat(", ?", len(q.Types)-1)+")")
	for _, t := range q.Types {
		args = append(args, t)
	}
	if q.MaxBytes > 0 {
		where = append(where, "m.file_length <= ?")
		args = append(args, q.MaxBytes)
	}
	if !q.Since.IsZero() {
		where = append(where, "m.timestamp >= ?")
		args = append(args, q.Since)
	}
	if q.After.MessageID != "" {
		where = append(where, "(m.timestamp, m.id, m.chat_jid) < (?, ?, ?)")
		args = append(args, q.After.ts, q.After.MessageID, q.After.ChatJID)
	}
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.chat_jid, m.media_type, COALESCE(m.filename, ''), m.file_length,
			CAST(m.timestamp AS TEXT)
		FROM messages m
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY m.timestamp DESC, m.id DESC, m.chat_jid DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AutoDownloadCandidate
	for rows.Next() {
		var c AutoDownloadCandidate
		if err := rows.Scan(&c.MessageID, &c.ChatJID, &c.MediaType, &c.Filename, &c.FileLength, &c.ts); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

package client

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/sealjay/mcp-whatsapp/internal/security"
)

// AutoDownloadMediaTypes lists the media types that can be downloaded
// automatically: the ones extractMediaInfo stores and Download can fetch.
var AutoDownloadMediaTypes = []string{"image", "video", "audio", "document"}

const (
	// DefaultAutoDownloadMaxMB is the default size cap for automatic
	// downloads. It sits above WhatsApp's long-standing 16 MB limit for
	// photos, videos and voice notes, so ordinary media always fits, while
	// large documents and long HD videos are left for download_media.
	// Download holds a whole file in memory, so the cap also bounds memory
	// at roughly workers x cap.
	DefaultAutoDownloadMaxMB = 25

	defaultAutoDownloadWorkers = 2
	defaultAutoDownloadQueue   = 4096
)

// AutoDownloadConfig controls automatic media download for incoming live
// and history-sync messages. The zero value disables it.
type AutoDownloadConfig struct {
	// Types is the set of media types to download (see
	// AutoDownloadMediaTypes). Empty disables auto-download.
	Types map[string]bool
	// MaxBytes skips media whose declared file length exceeds it. 0 means
	// no limit.
	MaxBytes uint64
	// BackfillDays limits the backfill pass (which fetches stored media the
	// queue dropped or never saw) to messages from the last N days. 0 means
	// no limit.
	BackfillDays int
	// Workers and QueueSize bound concurrency and pending work. Zero
	// values use the defaults (2 workers, 4096 queued downloads).
	Workers   int
	QueueSize int
}

// Enabled reports whether any media type is selected.
func (cfg AutoDownloadConfig) Enabled() bool { return len(cfg.Types) > 0 }

// TypeList returns the selected media types in a stable order, for logs.
func (cfg AutoDownloadConfig) TypeList() []string {
	out := make([]string, 0, len(cfg.Types))
	for t := range cfg.Types {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// ParseAutoDownloadTypes parses a comma-separated list of media types
// ("image,video", "all"). An empty spec returns an empty set (disabled).
func ParseAutoDownloadTypes(spec string) (map[string]bool, error) {
	types := map[string]bool{}
	for _, raw := range strings.Split(spec, ",") {
		t := strings.ToLower(strings.TrimSpace(raw))
		switch t {
		case "":
			continue
		case "all":
			for _, known := range AutoDownloadMediaTypes {
				types[known] = true
			}
		case "image", "video", "audio", "document":
			types[t] = true
		case "sticker":
			return nil, errors.New("sticker messages are not stored, so they cannot be auto-downloaded")
		default:
			return nil, fmt.Errorf("unknown media type %q (want a comma list of %s, or all)",
				raw, strings.Join(AutoDownloadMediaTypes, ","))
		}
	}
	return types, nil
}

// autoDownloadJob identifies one stored media message to fetch.
type autoDownloadJob struct {
	messageID  string
	chatJID    string
	mediaType  string
	fileLength uint64
	// pass is the backfill pass that queued the job; nil for jobs queued
	// as messages arrive. Workers report the job's outcome to it.
	pass *backfillPass
}

// autoDownloadOutcome is what became of one job, for backfill summaries.
type autoDownloadOutcome int

const (
	autoDownloadSkipped    autoDownloadOutcome = iota // not attempted or not kept (recorded failure, revoked, shutdown, store error)
	autoDownloadDone                                  // file is in the cache
	autoDownloadGone                                  // permanent failure, now recorded
	autoDownloadFailedSoft                            // transient failure, may succeed later
)

type autoDownloadKey struct{ messageID, chatJID string }

// autoDownloader runs media downloads off whatsmeow's event goroutine. It
// filters by type and declared size and dedupes requests that are already
// queued or in flight. enqueue never blocks: when the bounded queue is full
// the request is dropped with a warning and onDrop is called, so the
// backfill pass can pick the message up later. enqueueWait is the blocking
// variant for the backfill pass.
type autoDownloader struct {
	cfg      AutoDownloadConfig
	handle   func(context.Context, autoDownloadJob) autoDownloadOutcome
	log      waLog.Logger
	redactor *security.Redactor
	onDrop   func() // optional; set before the first enqueue

	jobs   chan autoDownloadJob
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	pending map[autoDownloadKey]struct{}
	stopped bool
	dropped uint64
}

// startAutoDownloader starts cfg.Workers workers that call handle for each
// queued job until ctx is cancelled or stop is called.
func startAutoDownloader(ctx context.Context, cfg AutoDownloadConfig, handle func(context.Context, autoDownloadJob) autoDownloadOutcome, log waLog.Logger, redactor *security.Redactor) *autoDownloader {
	if cfg.Workers <= 0 {
		cfg.Workers = defaultAutoDownloadWorkers
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultAutoDownloadQueue
	}
	ctx, cancel := context.WithCancel(ctx)
	a := &autoDownloader{
		cfg:      cfg,
		handle:   handle,
		log:      log,
		redactor: redactor,
		jobs:     make(chan autoDownloadJob, cfg.QueueSize),
		ctx:      ctx,
		cancel:   cancel,
		pending:  make(map[autoDownloadKey]struct{}),
	}
	for range cfg.Workers {
		a.wg.Add(1)
		go a.work(ctx)
	}
	return a
}

func (a *autoDownloader) work(ctx context.Context) {
	defer a.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-a.jobs:
			if ctx.Err() != nil {
				return
			}
			outcome := a.handle(ctx, job)
			a.mu.Lock()
			delete(a.pending, autoDownloadKey{job.messageID, job.chatJID})
			a.mu.Unlock()
			if job.pass != nil {
				job.pass.finish(outcome)
			}
		}
	}
}

// accepts reports whether job passes the type and size filters.
func (a *autoDownloader) accepts(job autoDownloadJob) bool {
	if !a.cfg.Types[job.mediaType] || job.messageID == "" {
		return false
	}
	if job.fileLength == 0 {
		// Download refuses media without a declared size.
		return false
	}
	if a.cfg.MaxBytes > 0 && job.fileLength > a.cfg.MaxBytes {
		a.log.Infof("Skipping auto-download of %s media for message %s: %d bytes exceeds the %d-byte limit",
			job.mediaType, a.redactor.MsgID(job.messageID), job.fileLength, a.cfg.MaxBytes)
		return false
	}
	return true
}

// enqueue queues job if it passes the type and size filters and is not
// already pending. It never blocks and reports whether the job was queued.
func (a *autoDownloader) enqueue(job autoDownloadJob) bool {
	if !a.accepts(job) {
		return false
	}

	key := autoDownloadKey{job.messageID, job.chatJID}
	a.mu.Lock()
	if a.stopped || a.ctx.Err() != nil {
		a.mu.Unlock()
		return false
	}
	if _, dup := a.pending[key]; dup {
		a.mu.Unlock()
		return false
	}
	select {
	case a.jobs <- job:
		a.pending[key] = struct{}{}
		a.mu.Unlock()
		return true
	default:
		a.dropped++
		// Warn on the first drop and then every 100th, so a large
		// history sync cannot flood the log.
		if a.dropped == 1 || a.dropped%100 == 0 {
			a.log.Warnf("Auto-download queue full (%d pending); skipped %s media for message %s in %s (%d skipped so far, the backfill pass will fetch them later)",
				cap(a.jobs), job.mediaType, a.redactor.MsgID(job.messageID), a.redactor.JID(job.chatJID), a.dropped)
		}
		a.mu.Unlock()
		if a.onDrop != nil {
			a.onDrop()
		}
		return false
	}
}

// enqueueWait is the blocking variant of enqueue for the backfill pass,
// which does not run on whatsmeow's event goroutine and so may wait for
// room in the queue. It returns false without queuing when the job is
// filtered out or already pending, and when ctx ends or the downloader
// stops before there is room.
func (a *autoDownloader) enqueueWait(ctx context.Context, job autoDownloadJob) bool {
	if !a.accepts(job) {
		return false
	}

	key := autoDownloadKey{job.messageID, job.chatJID}
	a.mu.Lock()
	if a.stopped || a.ctx.Err() != nil {
		a.mu.Unlock()
		return false
	}
	if _, dup := a.pending[key]; dup {
		a.mu.Unlock()
		return false
	}
	// Claim the key before waiting (workers need the lock to finish jobs,
	// so it cannot be held while blocked); a live copy of the same message
	// is then treated as a duplicate.
	a.pending[key] = struct{}{}
	a.mu.Unlock()

	select {
	case a.jobs <- job:
		return true
	case <-ctx.Done():
	case <-a.ctx.Done():
	}
	a.mu.Lock()
	delete(a.pending, key)
	a.mu.Unlock()
	return false
}

// stop cancels in-flight downloads, waits for the workers to exit and
// makes later enqueue calls no-ops. Queued jobs are discarded.
func (a *autoDownloader) stop() {
	a.mu.Lock()
	a.stopped = true
	a.mu.Unlock()
	a.cancel()
	a.wg.Wait()
}

// StartAutoDownload enables automatic media download for messages that
// arrive from now on, plus the backfill pass that fetches stored media the
// queue dropped or never saw (see requestAutoDownloadBackfill). Call it
// before StartEventHandler/Connect. The returned stop function cancels
// in-flight downloads and any running backfill pass and waits for both; it
// is a no-op when cfg selects no media type.
func (c *Client) StartAutoDownload(ctx context.Context, cfg AutoDownloadConfig) (stop func()) {
	if !cfg.Enabled() {
		return func() {}
	}
	a := startAutoDownloader(ctx, cfg, c.runAutoDownload, c.log, c.redactor)
	sched := newBackfillScheduler(a.ctx, func(ctx context.Context) {
		c.backfillAutoDownload(ctx, c.waConnected)
	})
	a.onDrop = func() { sched.request(backfillDebounce) }
	c.autoDownload, c.autoDownloadBackfill = a, sched
	return func() {
		a.stop()
		sched.stop()
	}
}

// queueAutoDownload hands a freshly stored media message to the
// auto-downloader, if one is running.
func (c *Client) queueAutoDownload(chatJID, messageID, mediaType string, fileLength uint64) {
	if c.autoDownload == nil || mediaType == "" {
		return
	}
	c.autoDownload.enqueue(autoDownloadJob{
		messageID:  messageID,
		chatJID:    chatJID,
		mediaType:  mediaType,
		fileLength: fileLength,
	})
}

// runAutoDownload fetches one message's media through Download, so the
// file lands in the same cache download_media reads (and a cached file is
// not fetched again). Media that is gone for good is recorded and logged
// once; it is never retried automatically. Jobs from a backfill pass log
// per-item failures at debug level only; the pass logs a summary.
func (c *Client) runAutoDownload(ctx context.Context, job autoDownloadJob) autoDownloadOutcome {
	msgID, chat := c.redactor.MsgID(job.messageID), c.redactor.JID(job.chatJID)
	infof, warnf := c.log.Infof, c.log.Warnf
	if job.pass != nil {
		infof, warnf = c.log.Debugf, c.log.Debugf
	}

	failed, err := c.store.HasAutoDownloadFailure(ctx, job.messageID, job.chatJID)
	if err != nil {
		c.log.Warnf("Auto-download of message %s in %s skipped: %v", msgID, chat, err)
		return autoDownloadSkipped
	}
	if failed {
		return autoDownloadSkipped
	}
	// A message deleted for everyone has its media cleared unless revoked
	// content is kept; there is nothing to fetch and nothing to report.
	if !c.keepRevoked {
		revoked, err := c.store.IsRevoked(ctx, job.chatJID, job.messageID)
		if err != nil {
			c.log.Warnf("Auto-download of message %s in %s skipped: %v", msgID, chat, err)
			return autoDownloadSkipped
		}
		if revoked {
			return autoDownloadSkipped
		}
	}

	res := c.Download(ctx, job.messageID, job.chatJID, "")
	if res.Success {
		return autoDownloadDone
	}
	if ctx.Err() != nil {
		// On shutdown the media is left for the next backfill pass.
		return autoDownloadSkipped
	}
	if errors.Is(res.err, errMessageRevoked) {
		// Deleted for everyone while it was fetched: nothing to keep.
		return autoDownloadSkipped
	}

	reason, permanent := classifyDownloadFailure(res.err)
	if !permanent {
		detail := ""
		if c.redactor.Debug {
			detail = ": " + c.redactor.Body(res.Message)
		}
		warnf("Auto-download of %s media for message %s in %s failed (%s)%s", job.mediaType, msgID, chat, reason, detail)
		return autoDownloadFailedSoft
	}
	if err := c.store.RecordAutoDownloadFailure(ctx, job.messageID, job.chatJID, reason, time.Now().UTC()); err != nil {
		c.log.Warnf("Failed to record auto-download failure for message %s in %s: %v", msgID, chat, err)
	}
	infof("Auto-download of %s media for message %s in %s gave up: %s", job.mediaType, msgID, chat, reason)
	return autoDownloadGone
}

// classifyDownloadFailure maps a Download failure to a log-safe reason and
// reports whether retrying could ever help. Expired CDN links (403, 404,
// 410), missing metadata and integrity failures are permanent; network
// errors, server errors and a dropped connection are not.
func classifyDownloadFailure(err error) (reason string, permanent bool) {
	var httpErr whatsmeow.DownloadHTTPError
	switch {
	case err == nil:
		return "download failed", false
	case errors.As(err, &httpErr) && httpErr.Response != nil &&
		(httpErr.StatusCode == 403 || httpErr.StatusCode == 404 || httpErr.StatusCode == 410):
		return fmt.Sprintf("media no longer available from WhatsApp (HTTP %d)", httpErr.StatusCode), true
	case errors.Is(err, errIncompleteMedia), errors.Is(err, whatsmeow.ErrNoURLPresent):
		return "incomplete media metadata", true
	case errors.Is(err, errInvalidMedia),
		errors.Is(err, whatsmeow.ErrInvalidMediaSHA256),
		errors.Is(err, whatsmeow.ErrInvalidMediaEncSHA256),
		errors.Is(err, whatsmeow.ErrInvalidMediaHMAC),
		errors.Is(err, whatsmeow.ErrTooShortFile):
		return "media failed integrity checks", true
	default:
		return "download failed", false
	}
}

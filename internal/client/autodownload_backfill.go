package client

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/sealjay/mcp-whatsapp/internal/store"
)

// The live path drops media when its queue is full (a first full history
// sync can store tens of thousands of messages in seconds) and never sees
// media stored before auto-download was enabled. A backfill pass scans the
// store for media of the selected types that is neither cached nor recorded
// as permanently failed, newest first, and feeds it to the same workers.
const (
	// backfillAfterConnect delays the pass after (re)connecting, so the
	// history-sync batches and offline messages that follow a connect
	// usually land before it starts.
	backfillAfterConnect = 10 * time.Second
	// backfillDebounce is how long after the last history-sync batch or
	// queue drop a pass starts; each new trigger restarts the wait.
	backfillDebounce = 45 * time.Second
	// backfillMaxWait bounds the debounce: a pass starts at most this long
	// after its first trigger, even if triggers keep arriving.
	backfillMaxWait = 10 * time.Minute

	backfillPageSize = 256
	// backfillWindow caps how many of a pass's jobs are queued or in
	// flight at once, so a pass never fills the queue the live path uses.
	backfillWindow = 16
)

// requestAutoDownloadBackfill asks for a backfill pass after delay. Repeated
// requests are debounced into one pass; a request during a pass schedules
// one more pass after it. It is a no-op unless auto-download is enabled.
func (c *Client) requestAutoDownloadBackfill(delay time.Duration) {
	if c.autoDownloadBackfill != nil {
		c.autoDownloadBackfill.request(delay)
	}
}

// waConnected reports whether whatsmeow holds a live connection; downloads
// fail while it does not, so a backfill pass waits for the next connect.
func (c *Client) waConnected() bool { return c.wa != nil && c.wa.IsConnected() }

// backfillStats counts what one backfill pass found and did.
type backfillStats struct {
	scanned     int // candidate rows read from the store
	cached      int // already in the media cache
	pending     int // already queued or in flight (live path)
	queued      int // handed to the workers by this pass
	downloaded  int
	unavailable int // permanent failures, now recorded
	failed      int // transient failures, tried again next pass
	skipped     int // not attempted (shutdown, store error)
}

// backfillPass tracks the jobs one pass has queued. slots is a semaphore of
// backfillWindow entries: acquiring one per queued job bounds the pass's
// share of the queue, and acquiring all of them waits for the pass's jobs
// to finish.
type backfillPass struct {
	slots chan struct{}

	mu    sync.Mutex
	stats backfillStats
}

func newBackfillPass() *backfillPass {
	return &backfillPass{slots: make(chan struct{}, backfillWindow)}
}

func (p *backfillPass) acquire(ctx context.Context) bool {
	select {
	case p.slots <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (p *backfillPass) release() { <-p.slots }

// finish records a queued job's outcome and frees its slot. Workers call it.
func (p *backfillPass) finish(outcome autoDownloadOutcome) {
	p.mu.Lock()
	switch outcome {
	case autoDownloadDone:
		p.stats.downloaded++
	case autoDownloadGone:
		p.stats.unavailable++
	case autoDownloadFailedSoft:
		p.stats.failed++
	default:
		p.stats.skipped++
	}
	p.mu.Unlock()
	p.release()
}

// wait blocks until every queued job has finished or ctx ends.
func (p *backfillPass) wait(ctx context.Context) bool {
	for range cap(p.slots) {
		if !p.acquire(ctx) {
			return false
		}
	}
	for range cap(p.slots) {
		p.release()
	}
	return true
}

// backfillAutoDownload runs one backfill pass: it pages through the store
// newest first and queues every candidate whose file is not cached,
// waiting for room in the queue, then waits for those downloads and logs
// one summary line. connected is checked before each page, so a pass
// started or interrupted while offline stops early; the next connect
// triggers a new one.
func (c *Client) backfillAutoDownload(ctx context.Context, connected func() bool) backfillStats {
	a := c.autoDownload
	if a == nil {
		return backfillStats{}
	}
	if !connected() {
		c.log.Infof("Auto-download backfill postponed: not connected to WhatsApp")
		return backfillStats{}
	}

	start := time.Now()
	q := store.AutoDownloadCandidateQuery{
		Types:          a.cfg.TypeList(),
		MaxBytes:       a.cfg.MaxBytes,
		Limit:          backfillPageSize,
		IncludeRevoked: c.keepRevoked,
	}
	if a.cfg.BackfillDays > 0 {
		q.Since = start.AddDate(0, 0, -a.cfg.BackfillDays)
	}

	pass := newBackfillPass()
	var stats backfillStats
	interrupted := ""
scan:
	for {
		if !connected() {
			interrupted = "connection lost"
			break
		}
		page, err := c.store.ListAutoDownloadCandidates(ctx, q)
		if err != nil {
			if ctx.Err() == nil {
				c.log.Warnf("Auto-download backfill: reading stored media failed: %v", err)
				interrupted = "store error"
			}
			break
		}
		for _, cand := range page {
			stats.scanned++
			if c.mediaCached(cand) {
				stats.cached++
				continue
			}
			if !pass.acquire(ctx) {
				break scan
			}
			job := autoDownloadJob{
				messageID:  cand.MessageID,
				chatJID:    cand.ChatJID,
				mediaType:  cand.MediaType,
				fileLength: cand.FileLength,
				pass:       pass,
			}
			if !a.enqueueWait(ctx, job) {
				pass.release()
				if ctx.Err() != nil {
					break scan
				}
				stats.pending++
				continue
			}
			stats.queued++
		}
		if len(page) < q.Limit {
			break
		}
		q.After = page[len(page)-1]
	}
	if ctx.Err() != nil || !pass.wait(ctx) {
		interrupted = "shutting down"
	}

	pass.mu.Lock()
	stats.downloaded, stats.unavailable, stats.failed, stats.skipped =
		pass.stats.downloaded, pass.stats.unavailable, pass.stats.failed, pass.stats.skipped
	pass.mu.Unlock()

	status := "finished"
	if interrupted != "" {
		status = "stopped (" + interrupted + ")"
	}
	c.log.Infof("Auto-download backfill %s after %s: %d stored media checked, %d already cached, %d already queued, %d queued: %d downloaded, %d no longer available, %d failed (retried next pass), %d not attempted",
		status, time.Since(start).Round(time.Second), stats.scanned, stats.cached, stats.pending, stats.queued,
		stats.downloaded, stats.unavailable, stats.failed, stats.skipped)
	return stats
}

// mediaCached reports whether cand's file is already where Download puts
// it, with the declared size. Download re-checks the hash before serving a
// cached file, so a size match is enough to skip the fetch here.
func (c *Client) mediaCached(cand store.AutoDownloadCandidate) bool {
	_, path, err := c.mediaCachePath(cand.MessageID, cand.ChatJID, cand.Filename)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			c.log.Debugf("Auto-download backfill: cannot inspect cached media for message %s: %s", c.redactor.MsgID(cand.MessageID), c.redactor.Err(err))
		}
		return false
	}
	return info.Mode().IsRegular() && uint64(info.Size()) == cand.FileLength
}

// backfillScheduler runs backfill passes one at a time. request debounces
// triggers (each restarts the wait, bounded by maxWait from the first) and
// a request that arrives during a pass schedules one more pass after it,
// so media dropped meanwhile is not missed.
type backfillScheduler struct {
	ctx      context.Context
	run      func(context.Context)
	debounce time.Duration // delay for the follow-up pass after a busy one
	maxWait  time.Duration

	mu      sync.Mutex
	timer   *time.Timer
	gen     uint64 // invalidates timers that were replaced or stopped
	firstAt time.Time
	running bool
	rerun   bool
	stopped bool
	wg      sync.WaitGroup
}

func newBackfillScheduler(ctx context.Context, run func(context.Context)) *backfillScheduler {
	return &backfillScheduler{ctx: ctx, run: run, debounce: backfillDebounce, maxWait: backfillMaxWait}
}

func (s *backfillScheduler) request(delay time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.ctx.Err() != nil {
		return
	}
	if s.running {
		s.rerun = true
		return
	}
	s.armLocked(delay)
}

func (s *backfillScheduler) armLocked(delay time.Duration) {
	now := time.Now()
	if s.timer == nil {
		s.firstAt = now
	} else {
		s.timer.Stop()
	}
	if s.maxWait > 0 {
		delay = min(delay, max(s.firstAt.Add(s.maxWait).Sub(now), 0))
	}
	s.gen++
	gen := s.gen
	s.timer = time.AfterFunc(delay, func() { s.fire(gen) })
}

func (s *backfillScheduler) fire(gen uint64) {
	s.mu.Lock()
	if gen != s.gen || s.stopped || s.running || s.ctx.Err() != nil {
		s.mu.Unlock()
		return
	}
	s.timer = nil
	s.running = true
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()

	s.run(s.ctx)

	s.mu.Lock()
	s.running = false
	if s.rerun && !s.stopped && s.ctx.Err() == nil {
		s.rerun = false
		s.armLocked(s.debounce)
	}
	s.mu.Unlock()
}

// stop cancels any pending pass and waits for a running one to return.
// Cancel the scheduler's context first, or a running pass runs to the end.
func (s *backfillScheduler) stop() {
	s.mu.Lock()
	s.stopped = true
	s.gen++
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.mu.Unlock()
	s.wg.Wait()
}

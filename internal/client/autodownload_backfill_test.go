package client

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"

	"github.com/sealjay/mcp-whatsapp/internal/security"
	"github.com/sealjay/mcp-whatsapp/internal/store"
)

func alwaysConnected() bool { return true }

// backfillFixture is a client on a real store with a helper to add media
// messages whose payload hash matches, so Download accepts fake payloads.
type backfillFixture struct {
	t        *testing.T
	c        *Client
	s        *store.Store
	payloads map[string][]byte // message ID -> payload
}

func newBackfillFixture(t *testing.T) *backfillFixture {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return &backfillFixture{t: t, c: newClientWithStore(t, s), s: s, payloads: map[string][]byte{}}
}

const backfillChat = "447700000099@s.whatsapp.net"

func (f *backfillFixture) add(id, mediaType string, age time.Duration, payload []byte) store.AutoDownloadCandidate {
	f.t.Helper()
	digest := sha256.Sum256(payload)
	ts := time.Now().Add(-age)
	if err := f.s.StoreChat(backfillChat, "test", ts); err != nil {
		f.t.Fatal(err)
	}
	filename := generatedMediaFilename(mediaType, digest[:], ".bin")
	if err := f.s.StoreMessage(context.Background(), store.Message{
		ID: id, ChatJID: backfillChat, Sender: backfillChat, Timestamp: ts,
		MediaType: mediaType, Filename: filename, URL: "https://example.invalid/" + id, DirectPath: "/" + id,
	}, []byte{1}, digest[:], []byte{2}, uint64(len(payload))); err != nil {
		f.t.Fatal(err)
	}
	f.payloads[id] = payload
	return store.AutoDownloadCandidate{MessageID: id, ChatJID: backfillChat, MediaType: mediaType, Filename: filename, FileLength: uint64(len(payload))}
}

// cache writes data where Download caches cand's media.
func (f *backfillFixture) cache(cand store.AutoDownloadCandidate, data []byte) {
	f.t.Helper()
	dir, path, err := f.c.mediaCachePath(cand.MessageID, cand.ChatJID, cand.Filename)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// recordingHandler completes every job at once and records the order.
type recordingHandler struct {
	mu  sync.Mutex
	ids []string
}

func (h *recordingHandler) handle(_ context.Context, job autoDownloadJob) autoDownloadOutcome {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ids = append(h.ids, job.messageID)
	return autoDownloadDone
}

func (h *recordingHandler) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.ids...)
}

func TestBackfill_NewestFirstAndSkips(t *testing.T) {
	f := newBackfillFixture(t)
	hour := time.Hour
	f.add("old", "image", 30*24*hour, []byte("old"))
	f.add("doc", "document", 3*hour, []byte("doc"))
	f.add("newest", "image", 1*hour, []byte("newest"))
	f.add("mid", "image", 5*hour, []byte("mid"))
	cached := f.add("cached", "image", 2*hour, []byte("cached"))
	f.cache(cached, []byte("cached"))
	stale := f.add("stale-cache", "image", 4*hour, []byte("stale-cache"))
	f.cache(stale, []byte("short")) // wrong size: fetch again
	f.add("failed", "image", 2*hour, []byte("failed"))
	if err := f.s.RecordAutoDownloadFailure(context.Background(), "failed", backfillChat, "expired (HTTP 410)", time.Now()); err != nil {
		t.Fatal(err)
	}
	f.add("big", "image", 2*hour, make([]byte, 2000))
	f.add("video", "video", 2*hour, []byte("video"))
	f.add("ancient", "image", 400*24*hour, []byte("ancient"))

	h := &recordingHandler{}
	f.c.autoDownload = startAutoDownloader(context.Background(), AutoDownloadConfig{
		Types:        map[string]bool{"image": true, "document": true},
		MaxBytes:     1000,
		BackfillDays: 90,
		Workers:      1, // one worker keeps the queue order observable
	}, h.handle, testLogger(), &security.Redactor{})
	defer f.c.autoDownload.stop()

	stats := f.c.backfillAutoDownload(context.Background(), alwaysConnected)

	want := []string{"newest", "doc", "stale-cache", "mid", "old"}
	if got := h.seen(); !reflect.DeepEqual(got, want) {
		t.Fatalf("download order = %v, want %v (newest first; cached, failed, oversize, unselected type and out-of-window skipped)", got, want)
	}
	if stats.scanned != 6 || stats.cached != 1 || stats.queued != 5 || stats.downloaded != 5 {
		t.Fatalf("stats = %+v, want 6 scanned, 1 cached, 5 queued, 5 downloaded", stats)
	}
}

// With the real download path: successes fill the cache, expired media is
// recorded, and a transient failure is tried again by the next pass while
// the others are not.
func TestBackfill_RealDownloadsAndSecondPass(t *testing.T) {
	f := newBackfillFixture(t)
	f.add("ok", "image", time.Hour, []byte("OK-BYTES"))
	f.add("gone", "image", 2*time.Hour, []byte("GONE"))
	f.add("flaky", "audio", 3*time.Hour, []byte("FLAKY"))

	var calls sync.Map // URL -> *atomic.Int32
	f.c.downloadMedia = func(_ context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error) {
		url := msg.(*MediaDownloader).URL
		n, _ := calls.LoadOrStore(url, new(atomic.Int32))
		n.(*atomic.Int32).Add(1)
		switch filepath.Base(url) {
		case "ok":
			return f.payloads["ok"], nil
		case "gone":
			return nil, whatsmeow.ErrMediaDownloadFailedWith410
		default:
			return nil, errors.New("failed to refresh media connections: websocket not connected")
		}
	}
	stop := f.c.StartAutoDownload(context.Background(), AutoDownloadConfig{Types: map[string]bool{"image": true, "audio": true}})
	defer stop()

	first := f.c.backfillAutoDownload(context.Background(), alwaysConnected)
	if first.queued != 3 || first.downloaded != 1 || first.unavailable != 1 || first.failed != 1 {
		t.Fatalf("first pass = %+v, want 3 queued: 1 downloaded, 1 unavailable, 1 failed", first)
	}
	second := f.c.backfillAutoDownload(context.Background(), alwaysConnected)
	if second.scanned != 2 || second.cached != 1 || second.queued != 1 || second.failed != 1 {
		t.Fatalf("second pass = %+v, want only the transient failure queued again", second)
	}
	for url, want := range map[string]int32{"https://example.invalid/ok": 1, "https://example.invalid/gone": 1, "https://example.invalid/flaky": 2} {
		n, _ := calls.Load(url)
		if n == nil || n.(*atomic.Int32).Load() != want {
			t.Errorf("%s fetched %v times, want %d", url, n, want)
		}
	}
}

func TestBackfill_WaitsForConnection(t *testing.T) {
	f := newBackfillFixture(t)
	f.add("m", "image", time.Hour, []byte("m"))
	h := &recordingHandler{}
	f.c.autoDownload = startAutoDownloader(context.Background(), AutoDownloadConfig{Types: map[string]bool{"image": true}},
		h.handle, testLogger(), &security.Redactor{})
	defer f.c.autoDownload.stop()

	if stats := f.c.backfillAutoDownload(context.Background(), func() bool { return false }); stats != (backfillStats{}) {
		t.Fatalf("offline pass did work: %+v", stats)
	}
	if got := h.seen(); len(got) != 0 {
		t.Fatalf("offline pass queued %v", got)
	}
}

// A pass blocked on busy workers returns promptly when the downloader stops.
func TestBackfill_StopsOnShutdown(t *testing.T) {
	f := newBackfillFixture(t)
	for i := range backfillWindow + 5 {
		f.add(string(rune('a'+i)), "image", time.Duration(i+1)*time.Minute, []byte{byte(i)})
	}
	h := newGatedHandler() // never released
	f.c.autoDownload = startAutoDownloader(context.Background(), AutoDownloadConfig{
		Types: map[string]bool{"image": true}, Workers: 1, QueueSize: 2,
	}, h.handle, testLogger(), &security.Redactor{})

	done := make(chan backfillStats)
	go func() { done <- f.c.backfillAutoDownload(f.c.autoDownload.ctx, alwaysConnected) }()
	h.waitStarted(t)
	f.c.autoDownload.stop()
	select {
	case stats := <-done:
		if stats.queued > 3 {
			t.Fatalf("queued %d jobs into a 1-worker, 2-slot queue", stats.queued)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("backfill pass did not return after stop")
	}
}

// enqueueWait blocks while the queue is full (backpressure) and proceeds
// once a worker frees a slot; the live enqueue still never blocks and
// reports the drop.
func TestAutoDownloader_EnqueueWaitBlocksUntilRoom(t *testing.T) {
	h := newGatedHandler()
	a := startAutoDownloader(context.Background(), AutoDownloadConfig{
		Types: map[string]bool{"image": true}, Workers: 1, QueueSize: 1,
	}, h.handle, testLogger(), &security.Redactor{})
	defer a.stop()
	var drops atomic.Int32
	a.onDrop = func() { drops.Add(1) }

	ctx := context.Background()
	if !a.enqueueWait(ctx, imageJob("first")) {
		t.Fatal("first rejected")
	}
	h.waitStarted(t)
	if !a.enqueueWait(ctx, imageJob("second")) { // fills the queue
		t.Fatal("second rejected")
	}

	done := make(chan bool)
	go func() { done <- a.enqueueWait(ctx, imageJob("third")) }()
	select {
	case <-done:
		t.Fatal("enqueueWait did not block on a full queue")
	case <-time.After(100 * time.Millisecond):
	}
	if a.enqueue(imageJob("third")) {
		t.Fatal("live enqueue of a message the backfill is waiting to queue was accepted")
	}
	if a.enqueue(imageJob("live")) || drops.Load() != 1 {
		t.Fatalf("live enqueue on a full queue: drops = %d, want 1", drops.Load())
	}

	close(h.release)
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("blocked enqueueWait gave up instead of queuing")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("enqueueWait never got room")
	}
	for range 2 {
		h.waitStarted(t) // second, third
	}
}

func TestAutoDownloader_EnqueueWaitGivesUp(t *testing.T) {
	h := newGatedHandler()
	a := startAutoDownloader(context.Background(), AutoDownloadConfig{
		Types: map[string]bool{"image": true}, Workers: 1, QueueSize: 1,
	}, h.handle, testLogger(), &security.Redactor{})
	a.enqueue(imageJob("busy"))
	h.waitStarted(t)
	a.enqueue(imageJob("queued"))

	// The caller's context ends: the claim on the key is released.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool)
	go func() { done <- a.enqueueWait(ctx, imageJob("waiting")) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if ok := <-done; ok {
		t.Fatal("enqueueWait queued after its context ended")
	}
	a.mu.Lock()
	_, claimed := a.pending[autoDownloadKey{"waiting", imageJob("").chatJID}]
	a.mu.Unlock()
	if claimed {
		t.Fatal("cancelled enqueueWait left its key pending")
	}

	// The downloader stops.
	go func() { done <- a.enqueueWait(context.Background(), imageJob("waiting")) }()
	time.Sleep(20 * time.Millisecond)
	a.stop()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("enqueueWait queued after stop")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("enqueueWait did not return after stop")
	}
	if a.enqueueWait(context.Background(), imageJob("late")) {
		t.Fatal("enqueueWait accepted after stop")
	}
}

// countingRun is a fake backfill pass: it counts runs and concurrent runs
// and, when gate is set, blocks until gate is closed or ctx ends.
type countingRun struct {
	gate    chan struct{}
	started chan struct{}

	mu      sync.Mutex
	runs    int
	running int
	peak    int
}

func newCountingRun(gated bool) *countingRun {
	r := &countingRun{started: make(chan struct{}, 16)}
	if gated {
		r.gate = make(chan struct{})
	}
	return r
}

func (r *countingRun) run(ctx context.Context) {
	r.mu.Lock()
	r.runs++
	r.running++
	r.peak = max(r.peak, r.running)
	r.mu.Unlock()
	r.started <- struct{}{}
	if r.gate != nil {
		select {
		case <-r.gate:
		case <-ctx.Done():
		}
	}
	r.mu.Lock()
	r.running--
	r.mu.Unlock()
}

func (r *countingRun) counts() (runs, peak int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs, r.peak
}

func (r *countingRun) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-r.started:
	case <-time.After(5 * time.Second):
		t.Fatal("backfill pass did not start")
	}
}

func TestBackfillScheduler_Debounces(t *testing.T) {
	r := newCountingRun(false)
	s := newBackfillScheduler(context.Background(), r.run)
	defer s.stop()

	start := time.Now()
	for range 5 {
		s.request(60 * time.Millisecond)
		time.Sleep(10 * time.Millisecond)
	}
	lastRequest := time.Now()
	r.waitStarted(t)
	if waited := time.Since(lastRequest); waited < 40*time.Millisecond {
		t.Fatalf("pass started %v after the last request (%v after the first), want the debounce to restart", waited, time.Since(start))
	}
	time.Sleep(150 * time.Millisecond)
	if runs, _ := r.counts(); runs != 1 {
		t.Fatalf("runs = %d, want 1 for a burst of requests", runs)
	}
}

func TestBackfillScheduler_MaxWait(t *testing.T) {
	r := newCountingRun(false)
	s := newBackfillScheduler(context.Background(), r.run)
	s.maxWait = 50 * time.Millisecond
	defer s.stop()

	stopRequests := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopRequests:
				return
			default:
				s.request(40 * time.Millisecond) // would postpone forever
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()
	defer close(stopRequests)
	r.waitStarted(t) // fails after 5 s if maxWait is not honoured
}

func TestBackfillScheduler_SingleFlightWithFollowUp(t *testing.T) {
	r := newCountingRun(true)
	s := newBackfillScheduler(context.Background(), r.run)
	s.debounce = 10 * time.Millisecond
	defer s.stop()

	s.request(0)
	r.waitStarted(t)
	for range 3 { // requests during a pass collapse into one follow-up
		s.request(0)
	}
	time.Sleep(50 * time.Millisecond)
	if runs, _ := r.counts(); runs != 1 {
		t.Fatalf("a second pass started while the first was running (runs = %d)", runs)
	}
	close(r.gate)
	r.waitStarted(t)
	time.Sleep(50 * time.Millisecond)
	if runs, peak := r.counts(); runs != 2 || peak != 1 {
		t.Fatalf("runs = %d, peak concurrency = %d; want 2 runs, never concurrent", runs, peak)
	}
}

func TestBackfillScheduler_Stop(t *testing.T) {
	// A pending pass is cancelled.
	r := newCountingRun(false)
	s := newBackfillScheduler(context.Background(), r.run)
	s.request(30 * time.Millisecond)
	s.stop()
	s.request(0)
	time.Sleep(80 * time.Millisecond)
	if runs, _ := r.counts(); runs != 0 {
		t.Fatalf("pass ran after stop (runs = %d)", runs)
	}

	// A running pass is waited for once its context ends.
	ctx, cancel := context.WithCancel(context.Background())
	r = newCountingRun(true)
	s = newBackfillScheduler(ctx, r.run)
	s.request(0)
	r.waitStarted(t)
	cancel()
	stopped := make(chan struct{})
	go func() { s.stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not return")
	}
	r.mu.Lock()
	running := r.running
	r.mu.Unlock()
	if running != 0 {
		t.Fatal("stop returned while a pass was still running")
	}
}

// StartAutoDownload wires queue drops to the scheduler; without
// auto-download, backfill requests are no-ops.
func TestStartAutoDownload_DropRequestsBackfill(t *testing.T) {
	c := newClientWithStore(t, newTestStoreWithLIDMap(t, nil))
	c.requestAutoDownloadBackfill(0) // disabled: must not panic

	stop := c.StartAutoDownload(context.Background(), AutoDownloadConfig{Types: map[string]bool{"image": true}})
	defer stop()
	if c.autoDownloadBackfill == nil || c.autoDownload.onDrop == nil {
		t.Fatal("backfill not wired")
	}
	c.autoDownload.onDrop()
	s := c.autoDownloadBackfill
	s.mu.Lock()
	armed := s.timer != nil
	s.mu.Unlock()
	if !armed {
		t.Fatal("a queue drop did not schedule a backfill pass")
	}
}

// The backfill pass passes keepRevoked through: a message deleted for
// everyone whose media an earlier -keep-revoked run kept is backfilled only
// while the server still keeps revoked content.
func TestBackfill_SkipsRevokedUnlessKept(t *testing.T) {
	for _, keep := range []bool{false, true} {
		f := newBackfillFixture(t)
		f.add("live", "image", time.Hour, []byte("live"))
		f.add("revoked", "image", 2*time.Hour, []byte("revoked"))
		if ok, err := f.s.ApplyRevoke(context.Background(), backfillChat, "revoked",
			store.Author{Senders: []string{backfillChat}}, time.Now(), true); err != nil || !ok {
			t.Fatalf("ApplyRevoke = %v, %v", ok, err)
		}
		f.c.keepRevoked = keep

		h := &recordingHandler{}
		f.c.autoDownload = startAutoDownloader(context.Background(), AutoDownloadConfig{
			Types: map[string]bool{"image": true}, Workers: 1,
		}, h.handle, testLogger(), &security.Redactor{})
		f.c.backfillAutoDownload(context.Background(), alwaysConnected)
		f.c.autoDownload.stop()

		want := []string{"live"}
		if keep {
			want = []string{"live", "revoked"}
		}
		if got := h.seen(); !reflect.DeepEqual(got, want) {
			t.Fatalf("keepRevoked=%v: backfilled %v, want %v", keep, got, want)
		}
	}
}

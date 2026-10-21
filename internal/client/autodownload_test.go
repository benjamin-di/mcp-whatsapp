package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"github.com/sealjay/mcp-whatsapp/internal/security"
	"github.com/sealjay/mcp-whatsapp/internal/store"
)

func TestParseAutoDownloadTypes(t *testing.T) {
	cases := []struct {
		spec    string
		want    []string
		wantErr bool
	}{
		{spec: "", want: nil},
		{spec: " , ", want: nil},
		{spec: "image", want: []string{"image"}},
		{spec: "Image, VIDEO ,audio", want: []string{"audio", "image", "video"}},
		{spec: "document,document", want: []string{"document"}},
		{spec: "all", want: []string{"audio", "document", "image", "video"}},
		{spec: "image,all", want: []string{"audio", "document", "image", "video"}},
		{spec: "sticker", wantErr: true},
		{spec: "image,gif", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			got, err := ParseAutoDownloadTypes(tc.spec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			cfg := AutoDownloadConfig{Types: got}
			if list := cfg.TypeList(); len(list)+len(tc.want) > 0 && !reflect.DeepEqual(list, tc.want) {
				t.Fatalf("types = %v, want %v", list, tc.want)
			}
			if cfg.Enabled() != (len(tc.want) > 0) {
				t.Fatalf("Enabled() = %v for %v", cfg.Enabled(), tc.want)
			}
		})
	}
}

func testLogger() waLog.Logger { return NewStderrLogger("test", "ERROR", false) }

// gatedHandler is a fake download handler: each call blocks until release is
// closed (or the context ends) and records the jobs it saw plus the peak
// number of concurrent calls.
type gatedHandler struct {
	release chan struct{}
	started chan autoDownloadJob

	mu      sync.Mutex
	seen    []autoDownloadJob
	running int
	peak    int
	ctxErrs int
}

func newGatedHandler() *gatedHandler {
	return &gatedHandler{release: make(chan struct{}), started: make(chan autoDownloadJob, 64)}
}

func (h *gatedHandler) handle(ctx context.Context, job autoDownloadJob) {
	h.mu.Lock()
	h.seen = append(h.seen, job)
	h.running++
	if h.running > h.peak {
		h.peak = h.running
	}
	h.mu.Unlock()
	h.started <- job

	select {
	case <-h.release:
	case <-ctx.Done():
		h.mu.Lock()
		h.ctxErrs++
		h.mu.Unlock()
	}

	h.mu.Lock()
	h.running--
	h.mu.Unlock()
}

func (h *gatedHandler) waitStarted(t *testing.T) autoDownloadJob {
	t.Helper()
	select {
	case job := <-h.started:
		return job
	case <-time.After(5 * time.Second):
		t.Fatal("handler was not called")
		return autoDownloadJob{}
	}
}

func imageJob(id string) autoDownloadJob {
	return autoDownloadJob{messageID: id, chatJID: "447700000001@s.whatsapp.net", mediaType: "image", fileLength: 1000}
}

func TestAutoDownloader_FiltersByTypeAndSize(t *testing.T) {
	h := newGatedHandler()
	close(h.release)
	a := startAutoDownloader(context.Background(), AutoDownloadConfig{
		Types:    map[string]bool{"image": true, "document": true},
		MaxBytes: 5000,
	}, h.handle, testLogger(), &security.Redactor{})
	defer a.stop()

	cases := []struct {
		name string
		job  autoDownloadJob
		want bool
	}{
		{"selected type", imageJob("ok-1"), true},
		{"at the limit", autoDownloadJob{messageID: "ok-2", chatJID: "c@s.whatsapp.net", mediaType: "document", fileLength: 5000}, true},
		{"type not selected", autoDownloadJob{messageID: "vid", chatJID: "c@s.whatsapp.net", mediaType: "video", fileLength: 10}, false},
		{"over the limit", autoDownloadJob{messageID: "big", chatJID: "c@s.whatsapp.net", mediaType: "document", fileLength: 5001}, false},
		{"unknown size", autoDownloadJob{messageID: "nosize", chatJID: "c@s.whatsapp.net", mediaType: "image"}, false},
		{"no message id", autoDownloadJob{chatJID: "c@s.whatsapp.net", mediaType: "image", fileLength: 10}, false},
	}
	for _, tc := range cases {
		if got := a.enqueue(tc.job); got != tc.want {
			t.Errorf("%s: enqueue = %v, want %v", tc.name, got, tc.want)
		}
	}
	h.waitStarted(t)
	h.waitStarted(t)
	select {
	case job := <-h.started:
		t.Fatalf("filtered job reached the handler: %+v", job)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestAutoDownloader_NoSizeLimit(t *testing.T) {
	h := newGatedHandler()
	close(h.release)
	a := startAutoDownloader(context.Background(), AutoDownloadConfig{Types: map[string]bool{"video": true}},
		h.handle, testLogger(), &security.Redactor{})
	defer a.stop()
	if !a.enqueue(autoDownloadJob{messageID: "huge", chatJID: "c@s.whatsapp.net", mediaType: "video", fileLength: 2 << 30}) {
		t.Fatal("MaxBytes 0 must not limit size")
	}
	h.waitStarted(t)
}

func TestAutoDownloader_DedupesPendingAndInFlight(t *testing.T) {
	h := newGatedHandler()
	a := startAutoDownloader(context.Background(), AutoDownloadConfig{
		Types: map[string]bool{"image": true}, Workers: 1, QueueSize: 8,
	}, h.handle, testLogger(), &security.Redactor{})
	defer a.stop()

	if !a.enqueue(imageJob("A")) {
		t.Fatal("first enqueue of A rejected")
	}
	h.waitStarted(t) // A is now in flight
	if a.enqueue(imageJob("A")) {
		t.Fatal("A was queued again while in flight")
	}
	if !a.enqueue(imageJob("B")) {
		t.Fatal("B rejected")
	}
	if a.enqueue(imageJob("B")) {
		t.Fatal("B was queued twice")
	}
	// Same message ID in another chat is a different message.
	other := imageJob("B")
	other.chatJID = "123456789@g.us"
	if !a.enqueue(other) {
		t.Fatal("B in another chat was treated as a duplicate")
	}

	close(h.release)
	h.waitStarted(t)
	h.waitStarted(t)
	// Once finished, the same message may be queued again (Download then
	// short-circuits on the cache or the recorded failure).
	deadline := time.Now().Add(5 * time.Second)
	for !a.enqueue(imageJob("A")) {
		if time.Now().After(deadline) {
			t.Fatal("A never left the pending set")
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.waitStarted(t)

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.seen) != 4 {
		t.Fatalf("handler calls = %d (%v), want 4", len(h.seen), h.seen)
	}
}

func TestAutoDownloader_BoundedQueueDropsWithoutBlocking(t *testing.T) {
	h := newGatedHandler()
	a := startAutoDownloader(context.Background(), AutoDownloadConfig{
		Types: map[string]bool{"image": true}, Workers: 2, QueueSize: 3,
	}, h.handle, testLogger(), &security.Redactor{})
	defer a.stop()

	// Two jobs occupy both workers, three fill the queue.
	for i := range 2 {
		if !a.enqueue(imageJob(fmt.Sprintf("busy-%d", i))) {
			t.Fatalf("busy-%d rejected", i)
		}
		h.waitStarted(t)
	}
	for i := range 3 {
		if !a.enqueue(imageJob(fmt.Sprintf("queued-%d", i))) {
			t.Fatalf("queued-%d rejected", i)
		}
	}

	done := make(chan bool)
	go func() { done <- a.enqueue(imageJob("overflow")) }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("overflow job was accepted by a full queue")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("enqueue blocked on a full queue")
	}
	a.mu.Lock()
	dropped := a.dropped
	_, overflowPending := a.pending[autoDownloadKey{"overflow", imageJob("").chatJID}]
	a.mu.Unlock()
	if dropped != 1 || overflowPending {
		t.Fatalf("dropped = %d, overflow pending = %v; want 1, false", dropped, overflowPending)
	}

	close(h.release)
	for range 3 {
		h.waitStarted(t)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.peak > 2 {
		t.Fatalf("peak concurrency = %d, want <= 2 workers", h.peak)
	}
	if len(h.seen) != 5 {
		t.Fatalf("handler calls = %d, want 5 (overflow must not run)", len(h.seen))
	}
}

func TestAutoDownloader_StopCancelsAndWaits(t *testing.T) {
	h := newGatedHandler() // never released: only cancellation ends a call
	a := startAutoDownloader(context.Background(), AutoDownloadConfig{
		Types: map[string]bool{"image": true}, Workers: 1, QueueSize: 4,
	}, h.handle, testLogger(), &security.Redactor{})

	a.enqueue(imageJob("in-flight"))
	h.waitStarted(t)
	a.enqueue(imageJob("queued"))

	stopped := make(chan struct{})
	go func() { a.stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not return")
	}

	h.mu.Lock()
	ctxErrs, seen, running := h.ctxErrs, len(h.seen), h.running
	h.mu.Unlock()
	if ctxErrs != 1 || running != 0 {
		t.Fatalf("in-flight call: ctx cancellations = %d, still running = %d; want 1, 0", ctxErrs, running)
	}
	if seen != 1 {
		t.Fatalf("handler calls = %d, want 1 (queued job must be discarded)", seen)
	}
	if a.enqueue(imageJob("late")) {
		t.Fatal("enqueue after stop was accepted")
	}
}

func TestAutoDownloader_ParentContextCancelStopsWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := newGatedHandler()
	a := startAutoDownloader(ctx, AutoDownloadConfig{Types: map[string]bool{"image": true}},
		h.handle, testLogger(), &security.Redactor{})
	cancel()
	if a.enqueue(imageJob("after-cancel")) {
		t.Fatal("enqueue accepted after the parent context ended")
	}
	a.stop() // must not hang
}

func TestClassifyDownloadFailure(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		permanent bool
	}{
		{"403", whatsmeow.ErrMediaDownloadFailedWith403, true},
		{"404 wrapped", fmt.Errorf("failed to download media from last host: %w", whatsmeow.ErrMediaDownloadFailedWith404), true},
		{"410", whatsmeow.ErrMediaDownloadFailedWith410, true},
		{"500", whatsmeow.DownloadHTTPError{Response: &http.Response{StatusCode: 500}}, false},
		{"incomplete", errIncompleteMedia, true},
		{"no url", whatsmeow.ErrNoURLPresent, true},
		{"validation", errInvalidMedia, true},
		{"enc hash", fmt.Errorf("failed to download media from last host: %w", whatsmeow.ErrInvalidMediaEncSHA256), true},
		{"network", errors.New("failed to refresh media connections: websocket not connected"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		reason, permanent := classifyDownloadFailure(tc.err)
		if permanent != tc.permanent || reason == "" {
			t.Errorf("%s: (%q, %v), want permanent=%v", tc.name, reason, permanent, tc.permanent)
		}
	}
}

// seedAutoDownloadMessage stores one image message with a real payload hash
// so Download's validation passes when the fake downloader returns payload.
func seedAutoDownloadMessage(t *testing.T, payload []byte) (*Client, *store.Store, autoDownloadJob, string) {
	t.Helper()
	t.Chdir(t.TempDir())
	s, err := store.Open("./store")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	const jid, msgID = "447700000099@s.whatsapp.net", "AUTO-1"
	digest := sha256.Sum256(payload)
	if err := s.StoreChat(jid, "test", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	filename := generatedMediaFilename("image", digest[:], ".jpg")
	if err := s.StoreMessage(context.Background(), store.Message{
		ID: msgID, ChatJID: jid, Sender: jid, Timestamp: time.Now().UTC(),
		MediaType: "image", Filename: filename, URL: "https://example.invalid/media", DirectPath: "/media?oe=1",
	}, []byte{1}, digest[:], []byte{2}, uint64(len(payload))); err != nil {
		t.Fatal(err)
	}
	cachePath, err := filepath.Abs(filepath.Join("store", jid, hex.EncodeToString([]byte(msgID)), filename))
	if err != nil {
		t.Fatal(err)
	}
	c := newClientWithStore(t, s)
	job := autoDownloadJob{messageID: msgID, chatJID: jid, mediaType: "image", fileLength: uint64(len(payload))}
	return c, s, job, cachePath
}

// The auto-download path must write exactly where download_media looks, so
// a later download_media call is a cache hit and does not fetch again.
func TestRunAutoDownload_SharesDownloadMediaCache(t *testing.T) {
	payload := []byte("JPEG-BYTES")
	c, _, job, cachePath := seedAutoDownloadMessage(t, payload)
	var calls atomic.Int32
	c.downloadMedia = func(context.Context, whatsmeow.DownloadableMessage) ([]byte, error) {
		calls.Add(1)
		return payload, nil
	}

	c.runAutoDownload(context.Background(), job)
	if got, err := os.ReadFile(cachePath); err != nil || string(got) != string(payload) {
		t.Fatalf("cache file = %q, %v; want payload at %s", got, err, cachePath)
	}

	res := c.Download(context.Background(), job.messageID, job.chatJID, "")
	if !res.Success || res.Path != cachePath {
		t.Fatalf("download_media after auto-download = %+v, want cache hit at %s", res, cachePath)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("media fetched %d times, want 1", n)
	}
}

func TestRunAutoDownload_ExpiredMediaRecordedAndNotRetried(t *testing.T) {
	c, s, job, cachePath := seedAutoDownloadMessage(t, []byte("GONE"))
	var calls atomic.Int32
	c.downloadMedia = func(context.Context, whatsmeow.DownloadableMessage) ([]byte, error) {
		calls.Add(1)
		return nil, whatsmeow.ErrMediaDownloadFailedWith410
	}

	c.runAutoDownload(context.Background(), job)
	c.runAutoDownload(context.Background(), job)
	if n := calls.Load(); n != 1 {
		t.Fatalf("expired media fetched %d times, want 1", n)
	}
	if failed, err := s.HasAutoDownloadFailure(context.Background(), job.messageID, job.chatJID); err != nil || !failed {
		t.Fatalf("failure not recorded: %v, %v", failed, err)
	}
	if _, err := os.Stat(cachePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache file should not exist: %v", err)
	}

	// download_media itself is not gated by the record.
	c.Download(context.Background(), job.messageID, job.chatJID, "")
	if n := calls.Load(); n != 2 {
		t.Fatalf("download_media did not try again (calls = %d)", n)
	}
}

func TestRunAutoDownload_TransientFailureNotRecorded(t *testing.T) {
	c, s, job, _ := seedAutoDownloadMessage(t, []byte("LATER"))
	c.downloadMedia = func(context.Context, whatsmeow.DownloadableMessage) ([]byte, error) {
		return nil, errors.New("failed to refresh media connections: websocket not connected")
	}
	c.runAutoDownload(context.Background(), job)
	if failed, err := s.HasAutoDownloadFailure(context.Background(), job.messageID, job.chatJID); err != nil || failed {
		t.Fatalf("transient failure recorded as permanent: %v, %v", failed, err)
	}
}

func TestRunAutoDownload_ShutdownNotRecorded(t *testing.T) {
	c, s, job, _ := seedAutoDownloadMessage(t, []byte("STOP"))
	ctx, cancel := context.WithCancel(context.Background())
	c.downloadMedia = func(ctx context.Context, _ whatsmeow.DownloadableMessage) ([]byte, error) {
		cancel()
		return nil, ctx.Err()
	}
	c.runAutoDownload(ctx, job)
	if failed, err := s.HasAutoDownloadFailure(context.Background(), job.messageID, job.chatJID); err != nil || failed {
		t.Fatalf("cancelled download recorded as a failure: %v, %v", failed, err)
	}
}

func TestStartAutoDownload_DisabledIsNoop(t *testing.T) {
	c := newClientWithStore(t, newTestStoreWithLIDMap(t, nil))
	stop := c.StartAutoDownload(context.Background(), AutoDownloadConfig{})
	defer stop()
	if c.autoDownload != nil {
		t.Fatal("auto-downloader started with no media types")
	}
	c.queueAutoDownload("c@s.whatsapp.net", "id", "image", 10) // must not panic
}

// A live media message is queued under its LID-resolved chat JID once it
// has been stored.
func TestHandleMessage_QueuesAutoDownload(t *testing.T) {
	s := newTestStoreWithLIDMap(t, map[string]string{"99887766": "447700000002"})
	c := newClientWithStore(t, s)
	const resolvedJID = "447700000002@s.whatsapp.net"
	if err := s.StoreChat(resolvedJID, "Bob", time.Now()); err != nil {
		t.Fatal(err)
	}

	h := newGatedHandler()
	close(h.release)
	c.autoDownload = startAutoDownloader(context.Background(), AutoDownloadConfig{Types: map[string]bool{"image": true}},
		h.handle, testLogger(), &security.Redactor{})
	defer c.autoDownload.stop()

	lid, err := types.ParseJID("99887766@lid")
	if err != nil {
		t.Fatal(err)
	}
	c.handleMessage(&events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: lid, Sender: lid},
			ID:            "img-1",
			Timestamp:     time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC),
		},
		Message: &waProto.Message{ImageMessage: &waProto.ImageMessage{
			FileSHA256: []byte{1, 2, 3},
			FileLength: proto.Uint64(2048),
			DirectPath: proto.String("/v/t62/img?oe=1"),
		}},
	})

	job := h.waitStarted(t)
	want := autoDownloadJob{messageID: "img-1", chatJID: resolvedJID, mediaType: "image", fileLength: 2048}
	if job != want {
		t.Fatalf("queued job = %+v, want %+v", job, want)
	}
}

// A message deleted for everyone before its queued auto-download runs is
// skipped: its media is cleared, so there is nothing to fetch or record.
func TestRunAutoDownload_SkipsRevokedMessage(t *testing.T) {
	c, s, job, cachePath := seedAutoDownloadMessage(t, []byte("JPEG-BYTES"))
	var calls atomic.Int32
	c.downloadMedia = func(context.Context, whatsmeow.DownloadableMessage) ([]byte, error) {
		calls.Add(1)
		return []byte("JPEG-BYTES"), nil
	}
	if _, err := c.revoke(context.Background(), job.chatJID, job.messageID, store.Author{Senders: []string{job.chatJID}}, time.Now()); err != nil {
		t.Fatal(err)
	}

	c.runAutoDownload(context.Background(), job)
	if n := calls.Load(); n != 0 {
		t.Fatalf("media fetched %d times for a revoked message, want 0", n)
	}
	if _, err := os.Stat(cachePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache file for revoked message: stat err = %v, want not exist", err)
	}
	if failed, err := s.HasAutoDownloadFailure(context.Background(), job.messageID, job.chatJID); err != nil || failed {
		t.Fatalf("HasAutoDownloadFailure = %v, %v; want false (skipped, not failed)", failed, err)
	}
}

// A revoke that lands while the media is being fetched must not leave the
// file behind. The fake fetch revokes the message and then recreates the
// cache directory, as when the revoke lands after Download read the media
// fields but before it created the directory. With -keep-revoked the file is
// kept.
func TestRunAutoDownload_RevokeDuringFetch(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(fmt.Sprintf("keepRevoked=%v", keep), func(t *testing.T) {
			payload := []byte("JPEG-BYTES")
			c, _, job, cachePath := seedAutoDownloadMessage(t, payload)
			c.keepRevoked = keep
			c.downloadMedia = func(ctx context.Context, _ whatsmeow.DownloadableMessage) ([]byte, error) {
				if _, err := c.revoke(ctx, job.chatJID, job.messageID, store.Author{Senders: []string{job.chatJID}}, time.Now()); err != nil {
					t.Error(err)
				}
				if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
					t.Error(err)
				}
				return payload, nil
			}

			c.runAutoDownload(context.Background(), job)
			_, err := os.Stat(cachePath)
			if keep && err != nil {
				t.Fatalf("keepRevoked: cache file missing: %v", err)
			}
			if !keep && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("cache file of message revoked during fetch: stat err = %v, want not exist", err)
			}
		})
	}
}

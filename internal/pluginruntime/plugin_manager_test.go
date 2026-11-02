package pluginruntime

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testHandler struct {
	handle func(context.Context, Event) error
	init   func(context.Context, Settings) error
	close  func(context.Context) error
}

func (h *testHandler) Handle(ctx context.Context, event Event) error {
	if h.handle != nil {
		return h.handle(ctx, event)
	}
	return nil
}
func (h *testHandler) Init(ctx context.Context, settings Settings) error {
	if h.init != nil {
		return h.init(ctx, settings)
	}
	return nil
}
func (h *testHandler) Close(ctx context.Context) error {
	if h.close != nil {
		return h.close(ctx)
	}
	return nil
}
func fixtureEvent() Event {
	return Event{VKMessageID: 42, VKPeerID: 2000000001, VKSenderID: 9, TelegramChatID: -123, TelegramMessageID: 1001, Text: "private text", PublishedAt: time.Unix(123, 0).UTC(), Attachments: []Attachment{{Kind: "photo", URL: "https://example.invalid/private", Name: "private.jpg"}}}
}
func startFixture(t *testing.T, names []string, registry *Registry, path string, timeout time.Duration) *Manager {
	t.Helper()
	manager, err := Start(context.Background(), Options{Names: names, Registry: registry, QueuePath: path, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeFixture(t, manager) })
	return manager
}
func closeFixture(t *testing.T, manager *Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not satisfied")
}
func doneJobs(m *Manager) int {
	var count int
	if err := m.db.QueryRow("SELECT count(*) FROM plugin_jobs WHERE payload IS NULL").Scan(&count); err != nil {
		return -1
	}
	return count
}

func TestDisabledDoesNotCreateStateOrFactory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.sqlite")
	registry := NewRegistry()
	registry.RegisterDurable("fixture", func() Handler { t.Error("disabled factory called"); return &testHandler{} })
	manager, err := Start(t.Context(), Options{Registry: registry, QueuePath: path})
	if err != nil || manager != nil {
		t.Fatal("disabled plugins must be a no-op")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("disabled plugins created state")
	}
}
func TestRegistryAndNames(t *testing.T) {
	for _, value := range []string{"a,,b", "A", "a,a", "../private", "a;token", ",a"} {
		if _, err := ParseNames(value); err == nil || strings.Contains(err.Error(), "token") {
			t.Fatalf("invalid names accepted or exposed")
		}
	}
	names, err := ParseNames(" first, second ")
	if err != nil || len(names) != 2 || names[0] != "first" || names[1] != "second" {
		t.Fatal("names not normalized")
	}
	r := NewRegistry()
	r.Register("first", func() Handler { return &testHandler{} })
	if _, err := r.resolve([]string{"missing"}); err == nil {
		t.Fatal("unknown plugin accepted")
	}
	if _, err := r.resolve([]string{"first", "first"}); err == nil {
		t.Fatal("duplicate enable accepted")
	}
	defer func() {
		if recover() == nil {
			t.Error("duplicate registration accepted")
		}
	}()
	r.Register("first", func() Handler { return &testHandler{} })
}
func TestEnqueueCopiesEventsAndDeduplicates(t *testing.T) {
	received := make(chan Event, 4)
	registry := NewRegistry()
	registry.RegisterDurable("fixture", func() Handler {
		return &testHandler{handle: func(_ context.Context, e Event) error { received <- e; return nil }}
	})
	path := filepath.Join(t.TempDir(), "queue.sqlite")
	manager := startFixture(t, []string{"fixture"}, registry, path, time.Second)
	event := fixtureEvent()
	if err := manager.Enqueue(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	event.Text = "changed"
	event.Attachments[0].Kind = "changed"
	eventually(t, func() bool { return doneJobs(manager) == 1 })
	got := <-received
	if got.Version != 1 || got.Text != "private text" || got.Attachments[0].Kind != "photo" || !got.PublishedAt.Equal(event.PublishedAt) {
		t.Fatal("event snapshot changed")
	}
	if err := manager.Enqueue(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	var count int
	manager.db.QueryRow("SELECT count(*) FROM plugin_jobs").Scan(&count)
	if count != 1 {
		t.Fatal("duplicate job inserted")
	}
	select {
	case <-received:
		t.Fatal("acknowledged event replayed")
	default:
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("unsafe queue permissions")
	}
}
func TestRetriesPersistAcrossRestartAndErasePayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.sqlite")
	registry := NewRegistry()
	registry.RegisterDurable("fixture", func() Handler {
		return &testHandler{handle: func(context.Context, Event) error { return errors.New("private failure") }}
	})
	manager := startFixture(t, []string{"fixture"}, registry, path, time.Second)
	if err := manager.Enqueue(t.Context(), fixtureEvent()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		var n int
		manager.db.QueryRow("SELECT attempts FROM plugin_jobs").Scan(&n)
		return n == 1
	})
	closeFixture(t, manager)
	recovered := make(chan Event, 1)
	registry = NewRegistry()
	registry.RegisterDurable("fixture", func() Handler {
		return &testHandler{handle: func(_ context.Context, e Event) error { recovered <- e; return nil }}
	})
	manager = startFixture(t, []string{"fixture"}, registry, path, time.Second)
	eventually(t, func() bool { return doneJobs(manager) == 1 })
	if (<-recovered).VKMessageID != 42 {
		t.Fatal("pending payload lost")
	}
	var payload []byte
	manager.db.QueryRow("SELECT payload FROM plugin_jobs").Scan(&payload)
	if payload != nil {
		t.Fatal("successful payload retained")
	}
}
func TestFailureAndPanicDoNotBlockOtherPluginsOrLeakLogs(t *testing.T) {
	var logs bytes.Buffer
	registry := NewRegistry()
	registry.RegisterDurable("failure", func() Handler {
		return &testHandler{handle: func(context.Context, Event) error { return errors.New("private failure text") }}
	})
	registry.RegisterDurable("panic", func() Handler {
		return &testHandler{handle: func(context.Context, Event) error { panic("private panic text") }}
	})
	registry.RegisterDurable("success", func() Handler { return &testHandler{} })
	manager, err := Start(context.Background(), Options{Names: []string{"failure", "panic", "success"}, Registry: registry, QueuePath: filepath.Join(t.TempDir(), "queue.sqlite"), Timeout: time.Second, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Enqueue(t.Context(), fixtureEvent()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		var n int
		manager.db.QueryRow("SELECT count(*) FROM plugin_jobs WHERE attempts > 0").Scan(&n)
		return n == 2 && doneJobs(manager) == 1
	})
	closeFixture(t, manager)
	for _, private := range []string{"private", "example.invalid"} {
		if strings.Contains(logs.String(), private) {
			t.Fatal("plugin log leaked payload/error")
		}
	}
}
func TestTimeoutQuarantinesHandlerAndShutdownIsBounded(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{}, 1)
	var calls, closed atomic.Int32
	registry := NewRegistry()
	registry.RegisterDurable("stuck", func() Handler {
		return &testHandler{handle: func(context.Context, Event) error { calls.Add(1); entered <- struct{}{}; <-release; return nil }, close: func(context.Context) error { closed.Add(1); return nil }}
	})
	registry.RegisterDurable("healthy", func() Handler { return &testHandler{} })
	manager := startFixture(t, []string{"stuck", "healthy"}, registry, filepath.Join(t.TempDir(), "queue.sqlite"), 100*time.Millisecond)
	if err := manager.Enqueue(t.Context(), fixtureEvent()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler not entered")
	}
	eventually(t, func() bool { return doneJobs(manager) == 1 })
	// A second event still reaches the healthy worker after the first times out.
	time.Sleep(150 * time.Millisecond)
	event := fixtureEvent()
	event.VKMessageID++
	if err := manager.Enqueue(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return doneJobs(manager) == 2 })
	closeFixture(t, manager)
	if calls.Load() != 1 || closed.Load() != 0 {
		t.Fatal("stuck handler was invoked again or closed concurrently")
	}
}
func TestInitializationFailureRetainsJobsWithoutStoppingQueue(t *testing.T) {
	var closed atomic.Bool
	registry := NewRegistry()
	registry.RegisterDurable("fixture", func() Handler {
		return &testHandler{init: func(context.Context, Settings) error { return errors.New("private") }, close: func(context.Context) error { closed.Store(true); return nil }}
	})
	manager := startFixture(t, []string{"fixture"}, registry, filepath.Join(t.TempDir(), "queue.sqlite"), time.Second)
	eventually(t, closed.Load)
	if err := manager.Enqueue(t.Context(), fixtureEvent()); err != nil {
		t.Fatal("failed plugin stopped durable capture", err)
	}
	var count int
	manager.db.QueryRow("SELECT count(*) FROM plugin_jobs WHERE payload IS NOT NULL").Scan(&count)
	if count != 1 {
		t.Fatal("pending job not retained")
	}
}
func TestQueueRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	os.WriteFile(target, []byte("untouched"), 0600)
	path := filepath.Join(directory, "queue.sqlite")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := openQueue(t.Context(), path); err == nil {
		t.Fatal("queue followed symlink")
	}
	contents, _ := os.ReadFile(target)
	if string(contents) != "untouched" {
		t.Fatal("symlink target modified")
	}
}

func TestLifecycleAndIndependentEventCopies(t *testing.T) {
	initialized := make(chan Settings, 2)
	received := make(chan Event, 2)
	var closed atomic.Int32
	registry := NewRegistry()
	registry.RegisterDurable("first", func() Handler {
		return &testHandler{
			init: func(_ context.Context, s Settings) error { initialized <- s; return os.MkdirAll(s.DataDir, 0700) },
			handle: func(_ context.Context, e Event) error {
				e.Text = "mutated"
				e.Attachments[0].Kind = "mutated"
				return nil
			},
			close: func(context.Context) error { closed.Add(1); return nil },
		}
	})
	registry.RegisterDurable("second", func() Handler {
		return &testHandler{
			init:   func(_ context.Context, s Settings) error { initialized <- s; return os.MkdirAll(s.DataDir, 0700) },
			handle: func(_ context.Context, e Event) error { received <- e; return nil },
			close:  func(context.Context) error { closed.Add(1); return nil },
		}
	})
	manager := startFixture(t, []string{"first", "second"}, registry, filepath.Join(t.TempDir(), "queue.sqlite"), time.Second)
	if err := manager.Enqueue(t.Context(), fixtureEvent()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return doneJobs(manager) == 2 })
	event := <-received
	if event.Text != "private text" || event.Attachments[0].Kind != "photo" {
		t.Fatal("one handler mutated another handler's event")
	}
	first, second := <-initialized, <-initialized
	if first.DataDir == second.DataDir {
		t.Fatal("plugins share a data directory")
	}
	for _, settings := range []Settings{first, second} {
		info, err := os.Stat(settings.DataDir)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			t.Fatal("invalid plugin data directory")
		}
	}
	closeFixture(t, manager)
	if closed.Load() != 2 {
		t.Fatal("lifecycle cleanup missing")
	}
}

func TestFactoryPanicIsContained(t *testing.T) {
	registry := NewRegistry()
	registry.RegisterDurable("broken", func() Handler { panic("private initialization payload") })
	registry.RegisterDurable("healthy", func() Handler { return &testHandler{} })
	manager := startFixture(t, []string{"broken", "healthy"}, registry, filepath.Join(t.TempDir(), "queue.sqlite"), time.Second)
	if err := manager.Enqueue(t.Context(), fixtureEvent()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return doneJobs(manager) == 1 })
}

// Reads may race with a worker writing a diagnostic record.
type lockedLogBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedLogBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}
func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}
func receiveEvent(t *testing.T, events <-chan Event) Event {
	t.Helper()
	select {
	case e := <-events:
		return e
	case <-time.After(3 * time.Second):
		t.Fatal("observer did not receive event")
		return Event{}
	}
}
func workerStopped(w *worker) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stopped
}

func TestMemoryObserversCopyEventsWithoutCreatingState(t *testing.T) {
	registry := NewRegistry()
	first, second := make(chan Event, 1), make(chan Event, 1)
	release := make(chan struct{})
	registry.Register("first", func() Handler {
		return &testHandler{handle: func(ctx context.Context, e Event) error {
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			e.Attachments[0].Kind = "mutated"
			first <- e
			return nil
		}}
	})
	registry.Register("second", func() Handler {
		return &testHandler{handle: func(ctx context.Context, e Event) error {
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			second <- e
			return nil
		}}
	})
	registry.RegisterDurable("disabled", func() Handler { t.Error("disabled factory invoked"); return &testHandler{} })
	directory := t.TempDir()
	manager := startFixture(t, []string{"first", "second"}, registry, filepath.Join(directory, "queue.sqlite"), time.Second)
	event := fixtureEvent()
	if err := manager.Enqueue(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	event.Text = "changed by producer"
	event.Attachments[0].Kind = "changed by producer"
	close(release)
	a, b := receiveEvent(t, first), receiveEvent(t, second)
	if a.Version != 1 || b.Version != 1 || a.Text != "private text" || b.Text != "private text" || b.Attachments[0].Kind != "photo" {
		t.Fatal("memory observers shared mutable event fields")
	}
	closeFixture(t, manager)
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 || manager.db != nil {
		t.Fatal("memory observers performed filesystem I/O")
	}
	if err := manager.Enqueue(t.Context(), fixtureEvent()); err == nil {
		t.Fatal("closed manager accepted an event")
	}
}

func TestMemoryOverflowDoesNotBlockOtherObservers(t *testing.T) {
	registry := NewRegistry()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	slow, healthy := make(chan Event, 3), make(chan Event, 3)
	registry.Register("slow", func() Handler {
		return &testHandler{handle: func(ctx context.Context, e Event) error {
			if e.VKMessageID == 42 {
				entered <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			slow <- e
			return nil
		}}
	})
	registry.Register("healthy", func() Handler {
		return &testHandler{handle: func(_ context.Context, e Event) error { healthy <- e; return nil }}
	})
	var logs lockedLogBuffer
	manager, err := Start(t.Context(), Options{Names: []string{"slow", "healthy"}, Registry: registry,
		MemoryQueueSize: 1, Timeout: 3 * time.Second, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeFixture(t, manager) })
	event := fixtureEvent()
	if err := manager.Enqueue(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("slow handler did not start")
	}
	receiveEvent(t, healthy)
	event.VKMessageID++
	if err := manager.Enqueue(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	receiveEvent(t, healthy)
	event.VKMessageID++
	if err := manager.Enqueue(t.Context(), event); err == nil {
		t.Fatal("full queue did not report dropped event")
	}
	if got := receiveEvent(t, healthy); got.VKMessageID != 44 {
		t.Fatal("full queue blocked healthy observer")
	}
	close(release)
	if receiveEvent(t, slow).VKMessageID != 42 || receiveEvent(t, slow).VKMessageID != 43 {
		t.Fatal("memory queue changed event order")
	}
	closeFixture(t, manager)
	if len(slow) != 0 || !strings.Contains(logs.String(), "event dropped") || strings.Contains(logs.String(), "private") {
		t.Fatal("overflow retried an event or leaked data")
	}
}

func TestMemoryFailuresAreNotRetried(t *testing.T) {
	registry := NewRegistry()
	handled := make(chan Event, 6)
	for _, name := range []string{"failure", "panic", "healthy"} {
		registry.Register(name, func() Handler {
			return &testHandler{handle: func(_ context.Context, e Event) error {
				handled <- e
				if e.VKMessageID == 42 {
					switch name {
					case "failure":
						return errors.New("private error payload")
					case "panic":
						panic("private panic payload")
					}
				}
				return nil
			}}
		})
	}
	var logs lockedLogBuffer
	manager, err := Start(t.Context(), Options{Names: []string{"failure", "panic", "healthy"}, Registry: registry, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeFixture(t, manager) })
	for id := int64(42); id <= 43; id++ {
		event := fixtureEvent()
		event.VKMessageID = id
		if err := manager.Enqueue(t.Context(), event); err != nil {
			t.Fatal(err)
		}
		for range 3 {
			if receiveEvent(t, handled).VKMessageID != id {
				t.Fatal("memory event retried")
			}
		}
	}
	// Wait for the error logs before shutdown, which can cancel an in-flight callback.
	eventually(t, func() bool { return strings.Count(logs.String(), "plugin handling failed; event dropped") == 2 })
	closeFixture(t, manager)
	if len(handled) != 0 || strings.Contains(logs.String(), "private") {
		t.Fatal("memory event retried or plugin error exposed")
	}
}

func TestMemoryTimeoutDisablesOnlyStuckObserver(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{}, 1)
	healthy := make(chan Event, 2)
	var closed atomic.Bool
	registry := NewRegistry()
	registry.Register("stuck", func() Handler {
		return &testHandler{
			handle: func(context.Context, Event) error { entered <- struct{}{}; <-release; return nil },
			close:  func(context.Context) error { closed.Store(true); return nil },
		}
	})
	registry.Register("healthy", func() Handler {
		return &testHandler{handle: func(_ context.Context, e Event) error { healthy <- e; return nil }}
	})
	manager := startFixture(t, []string{"stuck", "healthy"}, registry, "", 100*time.Millisecond)
	if err := manager.Enqueue(t.Context(), fixtureEvent()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	receiveEvent(t, healthy)
	eventually(t, func() bool { return workerStopped(manager.workers[0]) })
	event := fixtureEvent()
	event.VKMessageID++
	if err := manager.Enqueue(t.Context(), event); err == nil {
		t.Fatal("disabled observer silently accepted event")
	}
	receiveEvent(t, healthy)
	closeFixture(t, manager)
	if closed.Load() || len(entered) != 0 {
		t.Fatal("timed-out handler called concurrently")
	}
}

func TestMemoryWorkerInitializationFailure(t *testing.T) {
	for _, mode := range []string{"factory_panic", "init_error", "init_timeout"} {
		t.Run(mode, func(t *testing.T) {
			release := make(chan struct{})
			defer close(release)
			var closed atomic.Bool
			registry := NewRegistry()
			registry.Register("failed", func() Handler {
				if mode == "factory_panic" {
					panic("private")
				}
				return &testHandler{
					init: func(context.Context, Settings) error {
						if mode == "init_timeout" {
							<-release
						}
						return errors.New("private")
					},
					close: func(context.Context) error { closed.Store(true); return nil },
				}
			})
			manager := startFixture(t, []string{"failed"}, registry, "", 100*time.Millisecond)
			eventually(t, func() bool { return workerStopped(manager.workers[0]) })
			if err := manager.Enqueue(t.Context(), fixtureEvent()); err == nil {
				t.Fatal("failed observer accepted event")
			}
			closeFixture(t, manager)
			if closed.Load() != (mode == "init_error") {
				t.Fatal("unsafe or missing lifecycle cleanup")
			}
		})
	}
}

func TestObserverServiceOutlivesInitAndCloses(t *testing.T) {
	type startup struct {
		call     context.Context
		settings Settings
		server   *httptest.Server
	}
	started := make(chan startup, 1)
	var server *httptest.Server
	var closed atomic.Bool
	registry := NewRegistry()
	registry.Register("service", func() Handler {
		return &testHandler{
			init: func(ctx context.Context, settings Settings) error {
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if settings.Lifetime.Err() != nil {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					w.WriteHeader(http.StatusOK)
				}))
				started <- startup{ctx, settings, server}
				return nil
			},
			close: func(ctx context.Context) error { server.Close(); closed.Store(true); return ctx.Err() },
		}
	})
	manager := startFixture(t, []string{"service"}, registry, "", time.Second)
	var service startup
	select {
	case service = <-started:
	case <-time.After(time.Second):
		t.Fatal("service did not start")
	}
	eventually(t, func() bool { return service.call.Err() != nil })
	if service.settings.Lifetime.Err() != nil {
		t.Fatal("Init completion cancelled service lifetime")
	}
	client := service.server.Client()
	client.Timeout = time.Second
	response, err := client.Get(service.server.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal("observer service stopped after Init")
	}
	closeFixture(t, manager)
	if !closed.Load() || service.settings.Lifetime.Err() == nil {
		t.Fatal("service not stopped on manager shutdown")
	}
}

func TestMixedObserversAndUnavailableDurableStorage(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(map[bool]string{false: "healthy", true: "unavailable"}[broken], func(t *testing.T) {
			registry := NewRegistry()
			received := make(chan Event, 2)
			registry.Register("memory", func() Handler {
				return &testHandler{handle: func(_ context.Context, e Event) error { received <- e; return nil }}
			})
			registry.RegisterDurable("durable", func() Handler { return &testHandler{} })
			directory := t.TempDir()
			path := filepath.Join(directory, "queue.sqlite")
			if broken {
				path = directory
			} // Existing directory cannot be a SQLite file.
			manager := startFixture(t, []string{"durable", "memory"}, registry, path, time.Second)
			err := manager.Enqueue(t.Context(), fixtureEvent())
			if (err != nil) != broken {
				t.Fatal("unexpected durable capture result")
			}
			receiveEvent(t, received)
			if !broken {
				eventually(t, func() bool { return doneJobs(manager) == 1 })
				var name string
				if err := manager.db.QueryRow("SELECT plugin FROM plugin_jobs").Scan(&name); err != nil || name != "durable" {
					t.Fatal("memory observer stored a SQLite job")
				}
				// A later storage failure must not prevent memory delivery either.
				manager.db.Close()
				event := fixtureEvent()
				event.VKMessageID++
				if err := manager.Enqueue(t.Context(), event); err == nil {
					t.Fatal("closed database accepted durable job")
				}
				receiveEvent(t, received)
			}
		})
	}
}

func TestMemoryModeLeavesOldDurableJobsUntouched(t *testing.T) {
	registry := NewRegistry()
	registry.RegisterDurable("fixture", func() Handler {
		return &testHandler{handle: func(context.Context, Event) error { return errors.New("retry") }}
	})
	path := filepath.Join(t.TempDir(), "queue.sqlite")
	manager := startFixture(t, []string{"fixture"}, registry, path, time.Second)
	if err := manager.Enqueue(t.Context(), fixtureEvent()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		var attempts int
		manager.db.QueryRow("SELECT attempts FROM plugin_jobs").Scan(&attempts)
		return attempts > 0
	})
	closeFixture(t, manager)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan Event, 2)
	registry = NewRegistry()
	registry.Register("fixture", func() Handler {
		return &testHandler{handle: func(_ context.Context, e Event) error { received <- e; return nil }}
	})
	memory := startFixture(t, []string{"fixture"}, registry, path, time.Second)
	event := fixtureEvent()
	event.VKMessageID++
	if err := memory.Enqueue(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if receiveEvent(t, received).VKMessageID != 43 {
		t.Fatal("memory observer replayed old durable job")
	}
	closeFixture(t, memory)
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) || memory.db != nil {
		t.Fatal("memory mode changed old queue")
	}
	registry = NewRegistry()
	registry.RegisterDurable("fixture", func() Handler {
		return &testHandler{handle: func(_ context.Context, e Event) error { received <- e; return nil }}
	})
	recovered := startFixture(t, []string{"fixture"}, registry, path, time.Second)
	eventually(t, func() bool { return doneJobs(recovered) == 1 })
	if receiveEvent(t, received).VKMessageID != 42 {
		t.Fatal("old durable job lost")
	}
}

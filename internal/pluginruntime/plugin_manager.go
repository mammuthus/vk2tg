package pluginruntime

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

type Options struct {
	Names           []string
	QueuePath       string // Only opened when a RegisterDurable observer is enabled.
	DataDir         string // Optional root for plugin-owned state; not created by Start.
	MemoryQueueSize int    // Per observer; defaults to 64 pending events.
	Timeout         time.Duration
	Logger          *slog.Logger
	Registry        *Registry // nil uses implementations registered with Register.
}

type worker struct {
	name string
	registration
	events  chan Event
	wake    chan struct{}
	mu      sync.Mutex
	stopped bool
}

// stop releases pending memory snapshots and prevents enqueue after worker exit.
func (w *worker) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	for len(w.events) > 0 {
		<-w.events
	}
}

func (w *worker) enqueue(event Event) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return false
	}
	event.Attachments = slices.Clone(event.Attachments)
	select {
	case w.events <- event:
		return true
	default:
		return false
	}
}

type Manager struct {
	db           *sql.DB
	durableNames []string
	workers      []*worker
	timeout      time.Duration
	logger       *slog.Logger
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
}

// Start creates no state with no enabled observers or only memory observers.
// Observer implementations may create their own resources in Init.
// Only one Manager may consume a durable queue. The relay starts observers only
// for live, non-dry-run delivery.
func Start(ctx context.Context, options Options) (*Manager, error) {
	if len(options.Names) == 0 {
		return nil, nil
	}
	registry := options.Registry
	if registry == nil {
		registry = defaultRegistry
	}
	registrations, err := registry.resolve(options.Names)
	if err != nil {
		return nil, err
	}
	if options.Timeout == 0 {
		options.Timeout = 10 * time.Second
	}
	if options.Timeout < 0 {
		return nil, errors.New("plugin timeout must be positive")
	}
	if options.MemoryQueueSize == 0 {
		options.MemoryQueueSize = 64
	}
	if options.MemoryQueueSize < 0 {
		return nil, errors.New("plugin memory queue size must be positive")
	}
	if options.Logger == nil {
		options.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if options.DataDir == "" && options.QueuePath != "" {
		options.DataDir = filepath.Join(filepath.Dir(options.QueuePath), "plugins")
	}
	workerCtx, cancel := context.WithCancel(ctx)
	m := &Manager{timeout: options.Timeout, logger: options.Logger, ctx: workerCtx, cancel: cancel, done: make(chan struct{})}
	for i, name := range options.Names {
		w := &worker{name: name, registration: registrations[i]}
		if w.durable {
			m.durableNames = append(m.durableNames, name)
			w.wake = make(chan struct{}, 1)
		} else {
			w.events = make(chan Event, options.MemoryQueueSize)
		}
		m.workers = append(m.workers, w)
	}
	if len(m.durableNames) > 0 {
		m.db, err = openQueue(ctx, options.QueuePath)
		if err != nil {
			// Memory observers must remain usable even if durable storage fails.
			m.logger.Warn("durable plugin queue unavailable; durable observers disabled")
		}
	}
	var workers sync.WaitGroup
	for _, w := range m.workers {
		if w.durable && m.db == nil {
			w.stop()
			continue
		}
		directory := ""
		if options.DataDir != "" {
			directory = filepath.Join(options.DataDir, w.name)
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			m.work(workerCtx, w, directory)
		}()
	}
	go func() {
		<-workerCtx.Done()
		workers.Wait()
		if m.db != nil && m.db.Close() != nil {
			m.logger.Warn("plugin queue close failed")
		}
		close(m.done)
	}()
	return m, nil
}

// Enqueue runs after the Telegram mapping is committed. Memory observers get
// independent snapshots without waiting for their handlers. Durable jobs are
// captured separately; failure must never retry an already successful delivery.
func (m *Manager) Enqueue(ctx context.Context, event Event) error {
	if m == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if m.ctx.Err() != nil {
		return errors.New("plugin manager is closed")
	}
	if event.VKMessageID <= 0 || event.VKPeerID == 0 || event.TelegramChatID == 0 || event.TelegramMessageID <= 0 {
		return errors.New("invalid plugin event identifiers")
	}
	event.Version = 1
	var result error
	for _, w := range m.workers {
		if !w.durable && !w.enqueue(event) {
			m.logger.Warn("plugin memory queue full or worker stopped; event dropped", "plugin", w.name)
			result = errors.New("plugin event dropped")
		}
	}
	if len(m.durableNames) > 0 {
		result = errors.Join(result, m.enqueueDurable(ctx, event))
	}
	return result
}

func (m *Manager) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.cancel()
	select {
	case <-m.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var errPluginPanic = errors.New("plugin panicked")

// invoke bounds the wait even for handlers that ignore cancellation. A timed
// out worker is disabled for this process lifetime, so stuck calls cannot pile
// up and lifecycle methods are never called concurrently on that instance.
func invoke(parent context.Context, timeout time.Duration, callback func(context.Context) error) (err error, finished bool) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		completed := false
		defer func() {
			if !completed {
				recover()
				result <- errPluginPanic
			}
		}()
		err := callback(ctx)
		completed = true
		result <- err
	}()
	select {
	case err := <-result:
		return err, true
	case <-ctx.Done():
		return ctx.Err(), false
	}
}

func (m *Manager) work(parent context.Context, w *worker, directory string) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer w.stop()
	var handler Handler
	err, finished := invoke(ctx, m.timeout, func(operation context.Context) error {
		handler = w.factory()
		if handler == nil {
			return errors.New("nil plugin handler")
		}
		if initializer, ok := handler.(Initializer); ok {
			return initializer.Init(operation, Settings{DataDir: directory, Lifetime: ctx, Logger: m.logger.With("plugin", w.name)})
		}
		return nil
	})
	if !finished {
		m.logger.Warn("plugin initialization timed out or cancelled", "plugin", w.name)
		return
	}
	safeClose := true
	defer func() {
		cancel()
		if !safeClose {
			return
		}
		if closer, ok := handler.(Closer); ok {
			err, _ := invoke(context.Background(), min(m.timeout, 5*time.Second), closer.Close)
			if err != nil {
				m.logger.Warn("plugin close failed", "plugin", w.name)
			}
		}
	}()
	if err != nil {
		m.logger.Warn("plugin initialization failed", "plugin", w.name)
		return
	}
	if w.durable {
		safeClose = m.workDurable(ctx, w, handler)
		return
	}
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case event := <-w.events:
			err, finished := invoke(ctx, m.timeout, func(operation context.Context) error { return handler.Handle(operation, event) })
			if !finished {
				safeClose = false
				m.logger.Warn("plugin worker timed out or cancelled; worker disabled", "plugin", w.name)
				return
			}
			if err != nil {
				m.logger.Warn("plugin handling failed; event dropped", "plugin", w.name)
			}
		}
	}
}

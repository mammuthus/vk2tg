package plugins

import (
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mammuthus/vk2tg/internal/pluginruntime"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func init() {
	pluginruntime.Register("metrics", func() pluginruntime.Handler { return &metricsPlugin{} })
}

// metricsPlugin owns its registry and HTTP server. No global metrics or SQLite
// state are used. Lifecycle calls are serialized by the observer manager.
type metricsPlugin struct {
	listenAddr    string // Optional override for tests; production reads the environment.
	listener      net.Listener
	server        *http.Server
	serveDone     chan struct{}
	stopLifetime  func() bool
	messages      prometheus.Counter
	attachments   *prometheus.CounterVec
	lastRelay     prometheus.Gauge
	mu            sync.Mutex
	lastTimestamp float64
}

func (p *metricsPlugin) Init(ctx context.Context, settings pluginruntime.Settings) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lifetime := settings.Lifetime
	if lifetime == nil {
		lifetime = context.Background()
	}
	if err := lifetime.Err(); err != nil {
		return err
	}
	if p.server != nil {
		return errors.New("metrics observer already initialized")
	}
	address := p.listenAddr
	if address == "" {
		address = strings.TrimSpace(os.Getenv("METRICS_LISTEN_ADDR"))
	}
	if address == "" {
		address = "127.0.0.1:9091"
	}
	logger := settings.Logger
	if logger == nil {
		logger = slog.Default().With("plugin", "metrics")
	}

	registry := prometheus.NewRegistry()
	p.messages = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vk2tg_observed_messages_total",
		Help: "Successfully relayed source messages processed by the metrics observer.",
	})
	p.attachments = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vk2tg_observed_attachments_total",
		Help: "Normalized attachments in messages processed by the metrics observer, by kind.",
	}, []string{"kind"})
	for _, kind := range []string{"photo", "document", "sticker", "other"} {
		p.attachments.WithLabelValues(kind).Add(0)
	}
	p.lastRelay = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vk2tg_last_observed_relay_timestamp_seconds",
		Help: "Latest observed successful relay time as Unix seconds; zero before the first timestamped event.",
	})
	for _, collector := range []prometheus.Collector{
		p.messages, p.attachments, p.lastRelay,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	} {
		if err := registry.Register(collector); err != nil {
			return errors.New("cannot register metrics")
		}
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true, MaxRequestsInFlight: 5, Timeout: 3 * time.Second,
	}))
	server := &http.Server{
		Handler: mux, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 * 1024,
		// Avoid raw HTTP errors containing request details in production logs.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
	if err != nil {
		return errors.New("cannot listen for metrics")
	}
	if ctx.Err() != nil || lifetime.Err() != nil {
		listener.Close()
		return errors.New("metrics initialization cancelled")
	}
	p.listener, p.server, p.serveDone = listener, server, make(chan struct{})
	done := p.serveDone
	go func() {
		defer close(done)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics HTTP server stopped unexpectedly")
		}
	}()
	// Independent of Init's short-lived context; also releases the listener if
	// the manager cannot safely call Close after a timed-out callback.
	p.stopLifetime = context.AfterFunc(lifetime, func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = shutdownMetrics(shutdown, server)
	})
	logger.InfoContext(ctx, "metrics plugin initialized")
	return nil
}

func (p *metricsPlugin) Handle(ctx context.Context, event pluginruntime.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.server == nil {
		return errors.New("metrics observer is not initialized")
	}
	select {
	case <-p.serveDone:
		return errors.New("metrics HTTP server is unavailable")
	default:
	}
	if event.Version != 1 {
		return errors.New("unsupported metrics event version")
	}
	p.messages.Inc()
	for _, attachment := range event.Attachments {
		kind := attachment.Kind
		switch kind {
		case "photo", "document", "sticker":
		default:
			kind = "other"
		}
		p.attachments.WithLabelValues(kind).Inc()
	}
	if !event.RelayedAt.IsZero() && event.RelayedAt.Unix() > 0 {
		stamp := float64(event.RelayedAt.Unix()) + float64(event.RelayedAt.Nanosecond())/1e9
		p.mu.Lock()
		if stamp > p.lastTimestamp {
			p.lastTimestamp = stamp
			p.lastRelay.Set(stamp)
		}
		p.mu.Unlock()
	}
	return nil
}

func (p *metricsPlugin) Close(ctx context.Context) error {
	if p.stopLifetime != nil {
		p.stopLifetime()
	}
	if p.server == nil {
		return nil
	}
	err := shutdownMetrics(ctx, p.server)
	select {
	case <-p.serveDone:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func shutdownMetrics(ctx context.Context, server *http.Server) error {
	if err := server.Shutdown(ctx); err != nil {
		// Bound shutdown even if a client is still using a connection.
		_ = server.Close()
		return errors.New("metrics HTTP shutdown did not complete gracefully")
	}
	return nil
}

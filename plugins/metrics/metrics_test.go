package metrics

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mammuthus/vk2tg/internal/pluginruntime"
	"github.com/mammuthus/vk2tg/internal/plugintest"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

func startMetricsFixture(t *testing.T) *metricsPlugin {
	t.Helper()
	p := &metricsPlugin{listenAddr: "127.0.0.1:0"}
	initCtx, cancel := context.WithCancel(t.Context())
	if err := p.Init(initCtx, pluginruntime.Settings{Lifetime: t.Context(), Logger: slog.New(slog.DiscardHandler)}); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel() // Must not close the server after Init returns.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := p.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return p
}

func metricsURL(p *metricsPlugin) string { return "http://" + p.listener.Addr().String() + "/metrics" }

func scrapeMetrics(t *testing.T, p *metricsPlugin) (map[string]*dto.MetricFamily, string) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	response, err := client.Get(metricsURL(p))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("Content-Type"), "text/plain") {
		t.Fatal("invalid Prometheus HTTP response")
	}
	decoder := expfmt.NewDecoder(strings.NewReader(string(data)), expfmt.NewFormat(expfmt.TypeTextPlain))
	families := make(map[string]*dto.MetricFamily)
	for {
		family := new(dto.MetricFamily)
		err := decoder.Decode(family)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal("invalid Prometheus exposition", err)
		}
		families[family.GetName()] = family
	}
	return families, string(data)
}

func metricValue(t *testing.T, families map[string]*dto.MetricFamily, name, kind string) float64 {
	t.Helper()
	family := families[name]
	if family == nil {
		t.Fatalf("missing metric %s", name)
	}
	for _, metric := range family.Metric {
		if kind == "" && len(metric.Label) != 0 {
			continue
		}
		if kind != "" && (len(metric.Label) != 1 || metric.Label[0].GetName() != "kind" || metric.Label[0].GetValue() != kind) {
			continue
		}
		if metric.Counter != nil {
			return metric.Counter.GetValue()
		}
		if metric.Gauge != nil {
			return metric.Gauge.GetValue()
		}
	}
	t.Fatalf("missing metric series %s kind=%s", name, kind)
	return 0
}

func TestMetricsExpositionCountsAndPrivacy(t *testing.T) {
	p := startMetricsFixture(t)
	initial, _ := scrapeMetrics(t, p)
	if metricValue(t, initial, "vk2tg_observed_messages_total", "") != 0 || metricValue(t, initial, "vk2tg_last_observed_relay_timestamp_seconds", "") != 0 {
		t.Fatal("initial metrics must be zero")
	}
	for _, kind := range []string{"photo", "document", "sticker", "other"} {
		if metricValue(t, initial, "vk2tg_observed_attachments_total", kind) != 0 {
			t.Fatal("missing zero attachment series")
		}
	}
	if initial["go_goroutines"] == nil || initial["process_start_time_seconds"] == nil {
		t.Fatal("standard Go/process metrics missing")
	}
	event := plugintest.FixtureEvent()
	event.Version = 1
	event.RelayedAt = time.Unix(1700000000, 500000000)
	event.Attachments = []pluginruntime.Attachment{
		{Kind: "photo", URL: "https://private.invalid/photo", Name: "private-name"},
		{Kind: "photo"}, {Kind: "document"}, {Kind: "sticker"}, {Kind: "private-kind"},
	}
	if err := p.Handle(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	// One source message with many attachments still counts as one message.
	families, text := scrapeMetrics(t, p)
	if metricValue(t, families, "vk2tg_observed_messages_total", "") != 1 {
		t.Fatal("incorrect message count")
	}
	for kind, want := range map[string]float64{"photo": 2, "document": 1, "sticker": 1, "other": 1} {
		if metricValue(t, families, "vk2tg_observed_attachments_total", kind) != want {
			t.Fatal("incorrect attachment count")
		}
	}
	if len(families["vk2tg_observed_attachments_total"].Metric) != 4 {
		t.Fatal("unbounded attachment labels")
	}
	if metricValue(t, families, "vk2tg_last_observed_relay_timestamp_seconds", "") != 1700000000.5 {
		t.Fatal("wrong relay timestamp")
	}
	for _, secret := range []string{event.Text, "private.invalid", "private-name", "private-kind", "vk_message_id", "telegram_chat_id"} {
		if strings.Contains(text, secret) {
			t.Fatal("metrics exposed private event data")
		}
	}
	event.RelayedAt = time.Unix(1, 0)
	if err := p.Handle(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	event.RelayedAt = time.Time{}
	if err := p.Handle(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if !errors.Is(p.Handle(ctx, event), context.Canceled) {
		t.Fatal("cancelled Handle accepted event")
	}
	event.Version = 99
	if p.Handle(t.Context(), event) == nil {
		t.Fatal("unknown schema accepted")
	}
	families, _ = scrapeMetrics(t, p)
	if metricValue(t, families, "vk2tg_observed_messages_total", "") != 3 || metricValue(t, families, "vk2tg_last_observed_relay_timestamp_seconds", "") != 1700000000.5 {
		t.Fatal("invalid event counted or latest timestamp moved backwards")
	}
}

func TestMetricsHTTPRoutesAndOpenMetrics(t *testing.T) {
	p := startMetricsFixture(t)
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	for _, check := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/", 404}, {"GET", "/metrics/", 404}, {"POST", "/metrics", 405}, {"HEAD", "/metrics", 200},
	} {
		request, _ := http.NewRequest(check.method, "http://"+p.listener.Addr().String()+check.path, nil)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != check.status {
			t.Fatalf("unexpected response to %s %s", check.method, check.path)
		}
	}
	request, _ := http.NewRequest("GET", metricsURL(p), nil)
	request.Header.Set("Accept", "application/openmetrics-text; version=1.0.0")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || !strings.Contains(response.Header.Get("Content-Type"), "application/openmetrics-text") || !strings.HasSuffix(string(data), "# EOF\n") {
		t.Fatal("OpenMetrics negotiation failed")
	}
}

func TestMetricsConcurrentScrapesAndEvents(t *testing.T) {
	p := startMetricsFixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				event := plugintest.FixtureEvent()
				event.Version = 1
				event.RelayedAt = time.Unix(int64(100+j), 0)
				if err := p.Handle(t.Context(), event); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				scrapeMetrics(t, p)
			}
		}()
	}
	wg.Wait()
	families, _ := scrapeMetrics(t, p)
	if metricValue(t, families, "vk2tg_observed_messages_total", "") != 100 || metricValue(t, families, "vk2tg_observed_attachments_total", "photo") != 100 || metricValue(t, families, "vk2tg_last_observed_relay_timestamp_seconds", "") != 124 {
		t.Fatal("concurrent observations lost updates")
	}
}

func TestMetricsRegistryIsolationAndReset(t *testing.T) {
	first, second := startMetricsFixture(t), startMetricsFixture(t)
	event := plugintest.FixtureEvent()
	event.Version = 1
	if err := first.Handle(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	a, _ := scrapeMetrics(t, first)
	b, _ := scrapeMetrics(t, second)
	if metricValue(t, a, "vk2tg_observed_messages_total", "") != 1 || metricValue(t, b, "vk2tg_observed_messages_total", "") != 0 {
		t.Fatal("observers share a global metrics registry")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
	restarted := startMetricsFixture(t)
	c, _ := scrapeMetrics(t, restarted)
	if metricValue(t, c, "vk2tg_observed_messages_total", "") != 0 {
		t.Fatal("metrics survived restart unexpectedly")
	}
}

func TestMetricsCancellationAndListenerCleanup(t *testing.T) {
	p := startMetricsFixture(t)
	connection, err := net.DialTimeout("tcp", p.listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_, _ = connection.Write([]byte("GET /metrics HTTP/1.1\r\n")) // Incomplete client request.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_ = p.Close(ctx) // Must force-close even when graceful shutdown is cancelled.
	select {
	case <-p.serveDone:
	case <-time.After(time.Second):
		t.Fatal("HTTP server did not stop")
	}
	replacement, err := net.Listen("tcp", p.listener.Addr().String())
	if err != nil {
		t.Fatal("metrics listener not released", err)
	}
	replacement.Close()
	q := &metricsPlugin{listenAddr: "127.0.0.1:0"}
	if !errors.Is(q.Init(ctx, pluginruntime.Settings{}), context.Canceled) || q.listener != nil {
		t.Fatal("cancelled Init opened listener")
	}
	if q.Handle(t.Context(), plugintest.FixtureEvent()) == nil {
		t.Fatal("uninitialized observer accepted event")
	}
}

func TestMetricsLifetimeCancellation(t *testing.T) {
	lifetime, cancel := context.WithCancel(t.Context())
	defer cancel()
	p := &metricsPlugin{listenAddr: "127.0.0.1:0"}
	if err := p.Init(t.Context(), pluginruntime.Settings{Lifetime: lifetime, Logger: slog.New(slog.DiscardHandler)}); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-p.serveDone:
	case <-time.After(4 * time.Second):
		t.Fatal("lifetime cancellation left HTTP listener open")
	}
	ctx, done := context.WithTimeout(context.Background(), time.Second)
	defer done()
	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestMetricsDisabledAndInitializationFailureAreIsolated(t *testing.T) {
	if err := pluginruntime.Validate([]string{"metrics"}); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("METRICS_LISTEN_ADDR", listener.Addr().String())
	manager, err := pluginruntime.Start(t.Context(), pluginruntime.Options{})
	if err != nil || manager != nil {
		t.Fatal("disabled metrics tried to initialize")
	}
	registry := pluginruntime.NewRegistry()
	registry.Register("metrics", func() pluginruntime.Handler { return &metricsPlugin{} })
	received := make(chan pluginruntime.Event, 1)
	registry.Register("healthy", func() pluginruntime.Handler {
		return &plugintest.Handler{HandleFunc: func(_ context.Context, e pluginruntime.Event) error { received <- e; return nil }}
	})
	var logs plugintest.LogBuffer
	directory := t.TempDir()
	manager, err = pluginruntime.Start(t.Context(), pluginruntime.Options{Names: []string{"metrics", "healthy"}, Registry: registry,
		QueuePath: filepath.Join(directory, "queue.sqlite"), Logger: slog.New(slog.NewJSONHandler(&logs, nil)), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { plugintest.CloseFixture(t, manager) })
	plugintest.Eventually(t, func() bool { return strings.Contains(logs.String(), "plugin initialization failed") })
	_ = manager.Enqueue(t.Context(), plugintest.FixtureEvent())
	plugintest.ReceiveEvent(t, received)
	plugintest.CloseFixture(t, manager)
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("metrics created SQLite or plugin state")
	}
	if !strings.Contains(logs.String(), "plugin initialization failed") || strings.Contains(logs.String(), listener.Addr().String()) {
		t.Fatal("missing or unsafe initialization diagnostic")
	}
	p := &metricsPlugin{listenAddr: "private invalid address"}
	if err := p.Init(t.Context(), pluginruntime.Settings{}); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("invalid address accepted or leaked")
	}
}

func TestMetricsReceivesEventsThroughRealManager(t *testing.T) {
	// Let the observer bind a local ephemeral address through the runtime API.
	// A test-local factory captures the instance; no live bot or production state.
	t.Setenv("METRICS_LISTEN_ADDR", "127.0.0.1:0")
	instance := make(chan *metricsPlugin, 1)
	registry := pluginruntime.NewRegistry()
	registry.Register("metrics", func() pluginruntime.Handler { p := &metricsPlugin{}; instance <- p; return p })
	var logs plugintest.LogBuffer
	manager, err := pluginruntime.Start(t.Context(), pluginruntime.Options{Names: []string{"metrics"}, Registry: registry, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { plugintest.CloseFixture(t, manager) })
	var p *metricsPlugin
	select {
	case p = <-instance:
	case <-time.After(time.Second):
		t.Fatal("factory not called")
	}
	// The log buffer synchronizes Init completion before reading listener fields.
	plugintest.Eventually(t, func() bool { return strings.Contains(logs.String(), "metrics plugin initialized") })
	if err := manager.Enqueue(t.Context(), plugintest.FixtureEvent()); err != nil {
		t.Fatal(err)
	}
	plugintest.Eventually(t, func() bool {
		families, _ := scrapeMetrics(t, p)
		return metricValue(t, families, "vk2tg_observed_messages_total", "") == 1
	})
	address := p.listener.Addr().String()
	plugintest.CloseFixture(t, manager)
	connection, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
	if err == nil {
		connection.Close()
		t.Fatal("manager shutdown did not close metrics listener")
	}
}

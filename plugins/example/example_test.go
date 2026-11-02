package example

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mammuthus/vk2tg/internal/pluginruntime"
	"github.com/mammuthus/vk2tg/internal/plugintest"
)

func TestExampleLifecycleAndSafeMetadata(t *testing.T) {
	var output bytes.Buffer
	p := &examplePlugin{logger: slog.New(slog.NewJSONHandler(&output, nil)).With("plugin", "example")}
	if err := p.Init(t.Context(), pluginruntime.Settings{DataDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	event := plugintest.FixtureEvent()
	event.Version = 1
	event.Text = "Привет 🌍"
	event.NormalizedText = "private normalized text"
	if err := p.Handle(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n"))
	want := []map[string]any{
		{"msg": "example plugin initialized", "plugin": "example"},
		{"msg": "example plugin received event", "plugin": "example",
			"event_version": float64(1), "text_length": float64(8), "attachments_count": float64(1)},
		{"msg": "example plugin closed", "plugin": "example"},
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d log records, want %d", len(lines), len(want))
	}
	for i, line := range lines {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record["level"] != "INFO" || record["time"] == nil {
			t.Fatal("missing structured log metadata")
		}
		delete(record, "time")
		delete(record, "level")
		// An exact allowlist prevents accidental logging of private event fields.
		if !reflect.DeepEqual(record, want[i]) {
			t.Fatalf("unexpected fields in lifecycle record %d", i)
		}
	}
}

func TestExampleHonorsCancellation(t *testing.T) {
	var output bytes.Buffer
	p := &examplePlugin{logger: slog.New(slog.NewJSONHandler(&output, nil)).With("plugin", "example")}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, err := range []error{p.Init(ctx, pluginruntime.Settings{}), p.Handle(ctx, plugintest.FixtureEvent()), p.Close(ctx)} {
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancelled lifecycle call must return context cancellation")
		}
	}
	if output.Len() != 0 {
		t.Fatal("cancelled lifecycle calls logged a side effect")
	}
}

func TestExampleRegisteredAndProcessesEventWithoutSQLite(t *testing.T) {
	// Real compiled registry, synthetic event, and no API clients or credentials.
	if err := pluginruntime.Validate([]string{"example"}); err != nil {
		t.Fatal(err)
	}
	var output plugintest.LogBuffer
	path := filepath.Join(t.TempDir(), "queue.sqlite")
	manager, err := pluginruntime.Start(t.Context(), pluginruntime.Options{Names: []string{"example"}, QueuePath: path,
		Timeout: time.Second, Logger: slog.New(slog.NewJSONHandler(&output, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { plugintest.CloseFixture(t, manager) })
	if err := manager.Enqueue(t.Context(), plugintest.FixtureEvent()); err != nil {
		t.Fatal(err)
	}
	plugintest.Eventually(t, func() bool { return strings.Contains(output.String(), "example plugin received event") })
	plugintest.CloseFixture(t, manager)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("example created a database")
	}
	if !strings.Contains(output.String(), `"plugin":"example"`) || !strings.Contains(output.String(), "example plugin closed") {
		t.Fatal("example lifecycle or logger metadata missing")
	}
}

package plugins

import (
	"context"
	"log/slog"
	"unicode/utf8"

	"github.com/mammuthus/vk2tg/internal/pluginruntime"
)

// Register only the factory here: disabled plugins must not perform any I/O.
func init() {
	pluginruntime.Register("example", func() pluginruntime.Handler { return &examplePlugin{} })
}

// examplePlugin demonstrates an observer with no external services or own state.
// The manager serializes its lifecycle and Handle calls.
type examplePlugin struct {
	logger *slog.Logger
}

func (p *examplePlugin) Init(ctx context.Context, settings pluginruntime.Settings) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// pluginruntime.Settings.DataDir is available to plugins that need private local state.
	if p.logger == nil {
		p.logger = settings.Logger
		if p.logger == nil {
			p.logger = slog.Default().With("plugin", "example")
		}
	}
	p.logger.InfoContext(ctx, "example plugin initialized")
	return nil
}

func (p *examplePlugin) Handle(ctx context.Context, event pluginruntime.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Log only aggregate metadata, never text, IDs, filenames or URLs.
	// pluginruntime.Register selects best-effort delivery in memory, with no automatic retries.
	// Choose pluginruntime.RegisterDurable only when persistence and idempotent retries are needed.
	p.logger.InfoContext(ctx, "example plugin received event",
		"event_version", event.Version,
		"text_length", utf8.RuneCountInString(event.Text),
		"attachments_count", len(event.Attachments))
	return nil
}

func (p *examplePlugin) Close(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// This example owns no resources; real plugins release them here.
	if p.logger != nil {
		p.logger.InfoContext(ctx, "example plugin closed")
	}
	return nil
}

// Package pluginruntime provides opt-in, build-time observers of relayed messages.
// Implementations are trusted Go code, not sandboxed extensions.
package pluginruntime

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Event is a detached snapshot. It contains no bot tokens or API clients.
// Version is the persisted event schema version (currently 1).
// PublishedAt is zero when the source did not supply a valid timestamp.
type Event struct {
	Version                 int          `json:"version"`
	VKMessageID             int64        `json:"vk_message_id"`
	VKConversationMessageID int64        `json:"vk_conversation_message_id,omitempty"`
	VKPeerID                int64        `json:"vk_peer_id"`
	VKSenderID              int64        `json:"vk_sender_id"`
	VKReplyToID             int64        `json:"vk_reply_to_id,omitempty"`
	Out                     bool         `json:"out"`
	PublishedAt             time.Time    `json:"published_at"`
	RelayedAt               time.Time    `json:"relayed_at"`
	Text                    string       `json:"text"`
	NormalizedText          string       `json:"normalized_text"`
	Attachments             []Attachment `json:"attachments,omitempty"`
	UnsupportedAttachments  int          `json:"unsupported_attachments,omitempty"`
	IgnoredForwards         int          `json:"ignored_forwards,omitempty"`
	TelegramChatID          int64        `json:"telegram_chat_id"`
	TelegramMessageID       int64        `json:"telegram_message_id"`
}

type Attachment struct {
	Kind string `json:"kind"`
	URL  string `json:"url,omitempty"`
	Name string `json:"name,omitempty"`
}

// Handler observes successful deliveries; it cannot alter or block forwarding.
// Calls to an instance are serialized and must honor cancellation. RegisterDurable
// additionally requires idempotency: a completed side effect can be retried.
type Handler interface {
	Handle(context.Context, Event) error
}

// Initializer and Closer are optional lifecycle interfaces. Factories and init()
// should not perform I/O; do setup in Init, which receives a bounded context.
type Initializer interface {
	Init(context.Context, Settings) error
}
type Closer interface{ Close(context.Context) error }
type Settings struct {
	// DataDir is an optional private path. Create it with mode 0700 if needed.
	DataDir string
	// Lifetime lasts until this worker stops, unlike the short-lived Init context.
	// Use it for background services; release resources in Close.
	Lifetime context.Context
	Logger   *slog.Logger
}
type Factory func() Handler

type Registry struct {
	mu        sync.RWMutex
	factories map[string]registration
}

type registration struct {
	factory Factory
	durable bool
}

var defaultRegistry = NewRegistry()
var validName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

func NewRegistry() *Registry { return &Registry{factories: make(map[string]registration)} }

// Register is called by an implementation's init(). Invalid or duplicate
// registrations are programming errors and panic before the relay starts.
func Register(name string, factory Factory) { defaultRegistry.Register(name, factory) }
func (r *Registry) Register(name string, factory Factory) {
	r.register(name, factory, false)
}

// RegisterDurable opts an observer into SQLite persistence and retries.
// Register uses a bounded memory queue with no automatic retries instead.
func RegisterDurable(name string, factory Factory) { defaultRegistry.RegisterDurable(name, factory) }
func (r *Registry) RegisterDurable(name string, factory Factory) {
	r.register(name, factory, true)
}

func (r *Registry) register(name string, factory Factory, durable bool) {
	if !validName.MatchString(name) || factory == nil {
		panic("invalid plugin registration")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.factories[name]; exists {
		panic("duplicate plugin registration")
	}
	r.factories[name] = registration{factory: factory, durable: durable}
}

func ParseNames(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	names := strings.Split(value, ",")
	seen := make(map[string]bool)
	for i, name := range names {
		name = strings.TrimSpace(name)
		if !validName.MatchString(name) || seen[name] {
			return nil, errors.New("PLUGINS must contain distinct valid plugin names")
		}
		names[i], seen[name] = name, true
	}
	return names, nil
}

func Validate(names []string) error { _, err := defaultRegistry.resolve(names); return err }
func (r *Registry) resolve(names []string) ([]registration, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]registration, 0, len(names))
	seen := make(map[string]bool)
	for _, name := range names {
		factory, exists := r.factories[name]
		if !exists || seen[name] {
			return nil, errors.New("PLUGINS contains an unavailable or duplicate plugin")
		}
		seen[name] = true
		result = append(result, factory)
	}
	return result, nil
}

package main

import (
	"context"
	"time"
)

type telegramPacer struct {
	gate     chan struct{}
	next     time.Time
	interval time.Duration
	now      func() time.Time
	wait     func(context.Context, time.Duration) error
}

func newTelegramPacer() *telegramPacer {
	return &telegramPacer{
		gate: make(chan struct{}, 1), interval: time.Second,
		now: time.Now, wait: pause,
	}
}

func (pacer *telegramPacer) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case pacer.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (pacer *telegramPacer) release() { <-pacer.gate }

func (pacer *telegramPacer) reserve(ctx context.Context, messages int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay := pacer.next.Sub(pacer.now()); delay > 0 {
		if err := pacer.wait(ctx, delay); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	pacer.next = pacer.now().Add(time.Duration(messages) * pacer.interval)
	return nil
}

func (pacer *telegramPacer) postpone(delay time.Duration) {
	until := pacer.now().Add(delay)
	if until.After(pacer.next) {
		pacer.next = until
	}
}

func (pacer *telegramPacer) observeChat(chatType string, messages int) {
	if (chatType == "group" || chatType == "supergroup") && pacer.interval < 3*time.Second {
		pacer.next = pacer.next.Add(time.Duration(messages) * (3*time.Second - pacer.interval))
		pacer.interval = 3 * time.Second
	}
}

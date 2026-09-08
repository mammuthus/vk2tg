package main

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"
)

const (
	vkAPIMinInterval = 500 * time.Millisecond
	vkAPIMaxRetries  = 5
)

type vkRateStateStore interface {
	LoadVKRateState(context.Context) (time.Time, int, error)
	SaveVKRateState(context.Context, time.Time, int) error
}

type vkClock interface {
	Now() time.Time
	Wait(context.Context, time.Duration) error
}

type realVKClock struct{}

func (realVKClock) Now() time.Time                                      { return time.Now() }
func (realVKClock) Wait(ctx context.Context, delay time.Duration) error { return pause(ctx, delay) }

type vkRateGuard struct {
	mu             sync.Mutex
	clock          vkClock
	store          vkRateStateStore
	logger         *slog.Logger
	minimum        time.Duration
	maxRetries     int
	nextRequest    time.Time
	cooldownUntil  time.Time
	floodLevel     int
	expirationSeen bool
	jitter         func(time.Duration) time.Duration
}

func newVKRateGuard() *vkRateGuard {
	return &vkRateGuard{
		clock:      realVKClock{},
		logger:     slog.New(slog.DiscardHandler),
		minimum:    vkAPIMinInterval,
		maxRetries: vkAPIMaxRetries,
		jitter: func(delay time.Duration) time.Duration {
			spread := delay / 10
			if spread == 0 {
				return delay
			}
			return delay - spread + time.Duration(rand.Int64N(int64(2*spread)+1))
		},
	}
}

func (guard *vkRateGuard) configure(ctx context.Context, store vkRateStateStore, logger *slog.Logger) error {
	guard.mu.Lock()
	guard.store = store
	if logger != nil {
		guard.logger = logger
	}
	guard.mu.Unlock()
	if store == nil {
		return nil
	}
	until, level, err := store.LoadVKRateState(ctx)
	if err != nil {
		return err
	}
	guard.mu.Lock()
	guard.cooldownUntil = until
	guard.floodLevel = level
	active := guard.clock.Now().Before(until)
	guard.mu.Unlock()
	if active {
		guard.logger.Warn("VK flood cooldown restored from state", "until", until)
	}
	return nil
}

func (guard *vkRateGuard) wait(ctx context.Context) error {
	for {
		guard.mu.Lock()
		now := guard.clock.Now()
		allowed := guard.nextRequest
		if guard.cooldownUntil.After(allowed) {
			allowed = guard.cooldownUntil
		}
		if !guard.cooldownUntil.IsZero() && !now.Before(guard.cooldownUntil) && !guard.expirationSeen {
			guard.expirationSeen = true
			guard.logger.Info("VK flood cooldown expired")
		}
		if !now.Before(allowed) {
			guard.nextRequest = now.Add(guard.minimum)
			guard.mu.Unlock()
			return nil
		}
		delay := allowed.Sub(now)
		guard.mu.Unlock()
		if err := guard.clock.Wait(ctx, delay); err != nil {
			return err
		}
	}
}

func (guard *vkRateGuard) retry(ctx context.Context, attempt int, err error) error {
	if !retryableVKRequest(err) || attempt >= guard.maxRetries {
		return err
	}
	base := time.Second
	var apiError *VKAPIError
	if errors.As(err, &apiError) && apiError.Code == 6 {
		base = 2 * time.Second
	}
	delay := base << attempt
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	delay = guard.jitter(delay)
	guard.logger.Warn("VK API rate limited, retry in", "delay", delay)
	return guard.clock.Wait(ctx, delay)
}

func (guard *vkRateGuard) activateFlood(ctx context.Context) error {
	guard.mu.Lock()
	if guard.floodLevel < 3 {
		guard.floodLevel++
	}
	duration := 30 * time.Minute
	if guard.floodLevel == 2 {
		duration = time.Hour
	} else if guard.floodLevel >= 3 {
		duration = 2 * time.Hour
	}
	guard.cooldownUntil = guard.clock.Now().Add(duration)
	guard.expirationSeen = false
	until, level, store := guard.cooldownUntil, guard.floodLevel, guard.store
	guard.mu.Unlock()
	guard.logger.Warn("VK flood cooldown activated until", "until", until)
	if store != nil {
		return store.SaveVKRateState(ctx, until, level)
	}
	return nil
}

func (guard *vkRateGuard) successful(ctx context.Context) error {
	guard.mu.Lock()
	if guard.floodLevel == 0 || guard.clock.Now().Before(guard.cooldownUntil) {
		guard.mu.Unlock()
		return nil
	}
	guard.floodLevel = 0
	guard.cooldownUntil = time.Time{}
	guard.expirationSeen = false
	store := guard.store
	guard.mu.Unlock()
	if store != nil {
		return store.SaveVKRateState(ctx, time.Time{}, 0)
	}
	return nil
}

func retryableVKRequest(err error) bool {
	var apiError *VKAPIError
	if errors.As(err, &apiError) {
		return apiError.Code == 6
	}
	var httpError *VKHTTPError
	if errors.As(err, &httpError) {
		return httpError.StatusCode == 429 || httpError.StatusCode >= 500
	}
	var transportError *VKTransportError
	return errors.As(err, &transportError) && !errors.Is(err, context.Canceled)
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

type fakeVKClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
}

func (clock *fakeVKClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeVKClock) Wait(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clock.mu.Lock()
	clock.waits = append(clock.waits, delay)
	clock.now = clock.now.Add(delay)
	clock.mu.Unlock()
	return nil
}

func newRateTestClient(t *testing.T, handler http.HandlerFunc) (*VKClient, *fakeVKClock, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewVKClient(Config{VKAccessToken: "fake-token"}, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeVKClock{now: time.Unix(1_900_000_000, 0)}
	client.rate.clock = clock
	client.rate.minimum = vkAPIMinInterval
	client.rate.maxRetries = vkAPIMaxRetries
	client.rate.jitter = func(delay time.Duration) time.Duration { return delay }
	return client, clock, server
}

func TestVKAPIGlobalRateLimitAcrossMethods(t *testing.T) {
	var requestTimes []time.Time
	var clock *fakeVKClock
	client, fakeClock, _ := newRateTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		requestTimes = append(requestTimes, clock.Now())
		if request.URL.Path == "/users.get" {
			writeFixture(t, writer, `{"response":[]}`)
			return
		}
		writeFixture(t, writer, `{"response":{"server":"https://lp.test","key":"key","ts":"1"}}`)
	})
	clock = fakeClock
	if _, err := client.UsersGet(t.Context(), []int64{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetLongPollServer(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(requestTimes) != 2 || requestTimes[1].Sub(requestTimes[0]) < vkAPIMinInterval {
		t.Fatalf("API methods did not share limiter: %v", requestTimes)
	}
}

func TestVKLongPollWaitBypassesAPILimiter(t *testing.T) {
	requests := 0
	client, clock, server := newRateTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		requests++
		writeFixture(t, writer, `{"ts":"2","updates":[]}`)
	})
	client.rate.cooldownUntil = clock.Now().Add(30 * time.Minute)
	batch, err := client.WaitLongPoll(t.Context(), VKLongPollServer{Server: server.URL, Key: "key", TS: json.Number("1")})
	if err != nil || batch.TS.String() != "2" || requests != 1 || len(clock.waits) != 0 {
		t.Fatalf("long poll was rate limited: batch=%v requests=%d waits=%v err=%v", batch.TS, requests, clock.waits, err)
	}
}

func TestVKError6UsesExponentialBackoff(t *testing.T) {
	requests := 0
	client, clock, _ := newRateTestClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		if requests < 6 {
			writeFixture(t, writer, `{"error":{"error_code":6}}`)
			return
		}
		writeFixture(t, writer, `{"response":[]}`)
	})
	if _, err := client.UsersGet(t.Context(), []int64{1}); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second}
	if len(clock.waits) != len(want) {
		t.Fatalf("unexpected waits: %v", clock.waits)
	}
	for index := range want {
		if clock.waits[index] != want[index] {
			t.Fatalf("unexpected error 6 backoff: %v", clock.waits)
		}
	}
}

func TestVKTransientHTTPUsesBoundedBackoff(t *testing.T) {
	requests := 0
	client, clock, _ := newRateTestClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		if requests < 3 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeFixture(t, writer, `{"response":[]}`)
	})
	if _, err := client.UsersGet(t.Context(), []int64{1}); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{time.Second, 2 * time.Second}
	if len(clock.waits) != len(want) {
		t.Fatalf("unexpected transient waits: %v", clock.waits)
	}
	for index := range want {
		if clock.waits[index] != want[index] {
			t.Fatalf("unexpected transient backoff: %v", clock.waits)
		}
	}
}

func TestVKFloodCooldownEscalationAndCap(t *testing.T) {
	requests := 0
	client, clock, _ := newRateTestClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		writeFixture(t, writer, fmt.Sprintf(`{"error":{"error_code":%d}}`, []int{9, 9, 29, 9}[requests-1]))
	})
	want := []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour, 2 * time.Hour}
	for index, duration := range want {
		var result []VKUser
		err := client.call(t.Context(), "users.get", url.Values{}, &result)
		var apiError *VKAPIError
		if !errors.As(err, &apiError) || (apiError.Code != 9 && apiError.Code != 29) {
			t.Fatalf("missing typed flood error: %v", err)
		}
		if got := client.rate.cooldownUntil.Sub(clock.Now()); got != duration {
			t.Fatalf("cooldown %d: got %v want %v", index, got, duration)
		}
		if index < len(want)-1 {
			if err := clock.Wait(t.Context(), duration); err != nil {
				t.Fatal(err)
			}
		}
	}
	if requests != 4 {
		t.Fatalf("unexpected flood requests: %d", requests)
	}
}

func TestVKCooldownBlocksRequestAndHonorsCancellation(t *testing.T) {
	requests := 0
	client, clock, _ := newRateTestClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		writeFixture(t, writer, `{"response":[]}`)
	})
	client.rate.cooldownUntil = clock.Now().Add(30 * time.Minute)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.UsersGet(ctx, []int64{1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cooldown cancellation not propagated: %v", err)
	}
	if requests != 0 {
		t.Fatal("request sent during cooldown")
	}
}

func TestVKCooldownRestoreExpiryAndSuccessfulReset(t *testing.T) {
	store := testMessageStore(t)
	clock := &fakeVKClock{now: time.Unix(1_900_000_000, 0)}
	until := clock.Now().Add(30 * time.Minute)
	if err := store.SaveVKRateState(t.Context(), until, 2); err != nil {
		t.Fatal(err)
	}
	requests := 0
	client, _, _ := newRateTestClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		writeFixture(t, writer, `{"response":[]}`)
	})
	client.rate.clock = clock
	if err := client.ConfigureRateProtection(t.Context(), store, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.UsersGet(ctx, []int64{1}); !errors.Is(err, context.Canceled) || requests != 0 {
		t.Fatal("restored cooldown allowed a request")
	}
	clock.now = until
	if _, err := client.UsersGet(t.Context(), []int64{1}); err != nil || requests != 1 {
		t.Fatalf("expired cooldown blocked request: requests=%d err=%v", requests, err)
	}
	restoredUntil, level, err := store.LoadVKRateState(t.Context())
	if err != nil || !restoredUntil.IsZero() || level != 0 {
		t.Fatalf("successful request did not reset escalation: until=%v level=%d err=%v", restoredUntil, level, err)
	}
}

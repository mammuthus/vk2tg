package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestVKClientUsersGet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/method/users.get" {
			t.Error("unexpected HTTP method or VK method")
		}
		if request.URL.RawQuery != "" {
			t.Error("credentials and parameters must not be in the URL")
		}
		if err := request.ParseForm(); err != nil {
			t.Error("invalid form body")
		}
		if request.PostForm.Get("access_token") != "fake-vk-client-token" || request.PostForm.Get("v") != "5.199" || request.PostForm.Get("user_ids") != "42,73" {
			t.Error("unexpected VK parameters")
		}
		if _, err := writer.Write([]byte(`{"response":[{"id":42,"first_name":"Test","last_name":"User"}]}`)); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	client, err := NewVKClient(Config{VKAccessToken: "fake-vk-client-token"}, server.URL+"/method", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	users, err := client.UsersGet(t.Context(), []int64{42, 73})
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].ID != 42 || users[0].FirstName != "Test" || users[0].LastName != "User" {
		t.Fatalf("unexpected users: %+v", users)
	}
}

func TestVKClientErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		code   int
		kind   VKErrorKind
		cause  error
	}{
		{name: "authentication", code: 5, kind: VKErrorAuthentication},
		{name: "captcha", code: 14, kind: VKErrorCaptcha},
		{name: "validation", code: 17, kind: VKErrorValidation},
		{name: "manual action", code: 25, kind: VKErrorManualAction},
		{name: "generic API error", code: 100, kind: VKErrorGeneric},
		{name: "HTTP 500", status: 500, body: "fake-vk-client-token"},
		{name: "invalid JSON", body: `{"fake-vk-client-token":`, cause: ErrVKInvalidJSON},
		{name: "trailing JSON", body: `{"response":[]} {}`, cause: ErrVKInvalidJSON},
		{name: "invalid payload", body: `{"response":"fake-vk-client-token"}`, cause: ErrVKInvalidResponse},
		{name: "missing response", body: `{}`, cause: ErrVKInvalidResponse},
		{name: "null response", body: `{"response":null}`, cause: ErrVKInvalidResponse},
		{name: "invalid envelope", body: `[]`, cause: ErrVKInvalidResponse},
		{name: "invalid error code", body: `{"error":{"error_code":0}}`, cause: ErrVKInvalidResponse},
		{name: "ambiguous envelope", body: `{"response":[],"error":{"error_code":5}}`, cause: ErrVKInvalidResponse},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				body := testCase.body
				if testCase.code != 0 {
					body = fmt.Sprintf(`{"error":{"error_code":%d,"error_msg":"fake-vk-client-token fake-lp-key","request_params":[{"key":"access_token","value":"fake-vk-client-token"}]}}`, testCase.code)
				}
				if testCase.status != 0 {
					writer.WriteHeader(testCase.status)
				}
				if _, err := writer.Write([]byte(body)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			client, err := NewVKClient(Config{VKAccessToken: "fake-vk-client-token"}, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.UsersGet(t.Context(), []int64{42})
			if err == nil {
				t.Fatal("expected error")
			}
			if strings.Contains(err.Error(), "fake-vk-client-token") || strings.Contains(err.Error(), "fake-lp-key") {
				t.Fatal("error leaked private response data")
			}
			var apiError *VKAPIError
			var httpError *VKHTTPError
			switch {
			case testCase.code != 0:
				if !errors.As(err, &apiError) || apiError.Code != testCase.code || apiError.Kind() != testCase.kind || apiError.Message == "" {
					t.Fatalf("incorrect API error classification: %v", err)
				}
			case testCase.status != 0:
				if !errors.As(err, &httpError) || httpError.StatusCode != testCase.status || errors.As(err, &apiError) {
					t.Fatalf("incorrect HTTP error classification: %v", err)
				}
			default:
				if !errors.Is(err, testCase.cause) || errors.As(err, &apiError) {
					t.Fatalf("incorrect response error classification: %v", err)
				}
			}
		})
	}
}

func TestVKClientCancellation(t *testing.T) {
	tests := []struct {
		name       string
		timeout    time.Duration
		deadline   bool
		cancel     bool
		streamBody bool
		want       error
	}{
		{name: "client timeout", timeout: 50 * time.Millisecond, want: context.DeadlineExceeded},
		{name: "body timeout", timeout: 50 * time.Millisecond, streamBody: true, want: context.DeadlineExceeded},
		{name: "context deadline", timeout: time.Second, deadline: true, want: context.DeadlineExceeded},
		{name: "context cancellation", timeout: time.Second, cancel: true, want: context.Canceled},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if testCase.deadline {
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithTimeout(ctx, 50*time.Millisecond)
				defer deadlineCancel()
			}
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if testCase.streamBody {
					writer.WriteHeader(http.StatusOK)
					writer.(http.Flusher).Flush()
				}
				if testCase.cancel {
					cancel()
				}
				select {
				case <-request.Context().Done():
				case <-release:
				}
			}))
			defer server.Close()
			defer close(release)
			client, err := NewVKClient(Config{VKAccessToken: "fake-vk-client-token"}, server.URL, testCase.timeout)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.GetLongPollServer(ctx)
			var transportError *VKTransportError
			if !errors.As(err, &transportError) || !errors.Is(err, testCase.want) {
				t.Fatalf("expected transport error wrapping %v, got %v", testCase.want, err)
			}
			if strings.Contains(err.Error(), "fake-vk-client-token") {
				t.Fatal("transport error leaked token")
			}
		})
	}
}

func TestVKClientGetLongPollServer(t *testing.T) {
	for _, timestamp := range []string{`123456`, `"123456"`} {
		t.Run(timestamp, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPost || request.URL.Path != "/messages.getLongPollServer" {
					t.Error("unexpected HTTP method or VK method")
				}
				if err := request.ParseForm(); err != nil {
					t.Error("invalid form body")
				}
				if request.PostForm.Get("access_token") != "fake-vk-client-token" || request.PostForm.Get("v") != "5.199" {
					t.Error("missing token or API version")
				}
				if _, err := fmt.Fprintf(writer, `{"response":{"server":"lp.example.test","key":"fake-lp-key","ts":%s}}`, timestamp); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			client, err := NewVKClient(Config{VKAccessToken: "fake-vk-client-token"}, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.GetLongPollServer(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if response.Server != "lp.example.test" || response.Key != "fake-lp-key" || response.TS.String() != "123456" {
				t.Error("unexpected Long Poll server response")
			}
		})
	}
}

func TestVKClientConnectionFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {}))
	server.Close()
	client, err := NewVKClient(Config{VKAccessToken: "fake-vk-client-token"}, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.UsersGet(t.Context(), nil)
	var transportError *VKTransportError
	if !errors.As(err, &transportError) {
		t.Fatalf("expected transport error, got %v", err)
	}
	if strings.Contains(err.Error(), "fake-vk-client-token") || strings.Contains(err.Error(), server.URL) {
		t.Fatal("transport error exposed request details")
	}
}

func TestVKClientRejectsRedirect(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		redirected.Add(1)
	}))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Location", destination.URL+"/?access_token=fake-vk-client-token")
		writer.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client, err := NewVKClient(Config{VKAccessToken: "fake-vk-client-token"}, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.UsersGet(t.Context(), nil)
	var httpError *VKHTTPError
	if !errors.As(err, &httpError) || httpError.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("expected redirect HTTP error, got %v", err)
	}
	if redirected.Load() != 0 || strings.Contains(err.Error(), "fake-vk-client-token") {
		t.Fatal("redirect forwarded credentials or exposed them in error text")
	}
}

func TestVKClientInvalidLongPollResponse(t *testing.T) {
	for _, body := range []string{
		`{"response":{}}`,
		`{"response":{"key":"fake-lp-key","ts":123}}`,
		`{"response":{"server":"lp.example.test","ts":123}}`,
		`{"response":{"server":"lp.example.test","key":"fake-lp-key"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if _, err := writer.Write([]byte(body)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			client, err := NewVKClient(Config{VKAccessToken: "fake-vk-client-token"}, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.GetLongPollServer(t.Context())
			if !errors.Is(err, ErrVKInvalidResponse) {
				t.Fatalf("expected invalid response, got %v", err)
			}
		})
	}
}

func TestNewVKClientValidation(t *testing.T) {
	tests := []struct {
		name     string
		token    string
		endpoint string
		timeout  time.Duration
	}{
		{name: "missing token", timeout: time.Second},
		{name: "zero timeout", token: "fake-vk-client-token"},
		{name: "negative timeout", token: "fake-vk-client-token", timeout: -time.Second},
		{name: "invalid endpoint", token: "fake-vk-client-token", timeout: time.Second, endpoint: "://fake-vk-client-token"},
		{name: "credentials in endpoint", token: "fake-vk-client-token", timeout: time.Second, endpoint: "https://user:fake-vk-client-token@example.test"},
		{name: "query in endpoint", token: "fake-vk-client-token", timeout: time.Second, endpoint: "https://example.test?access_token=fake-vk-client-token"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := NewVKClient(Config{VKAccessToken: testCase.token}, testCase.endpoint, testCase.timeout)
			if err == nil {
				t.Fatal("expected invalid client configuration error")
			}
			if strings.Contains(err.Error(), "fake-vk-client-token") {
				t.Fatal("configuration error leaked token")
			}
		})
	}
}

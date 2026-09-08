package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const VKAPIVersion = "5.199"

var (
	ErrVKInvalidJSON     = errors.New("invalid vk JSON response")
	ErrVKInvalidResponse = errors.New("invalid vk response")
)

type VKErrorKind string

const (
	VKErrorGeneric        VKErrorKind = "api_error"
	VKErrorAuthentication VKErrorKind = "authentication"
	VKErrorCaptcha        VKErrorKind = "captcha"
	VKErrorValidation     VKErrorKind = "validation"
	VKErrorManualAction   VKErrorKind = "manual_action"
)

type VKAPIError struct {
	Code    int
	Message string
}

func (apiError *VKAPIError) Error() string {
	return fmt.Sprintf("vk API error %d: %s", apiError.Code, apiError.Message)
}

func (apiError *VKAPIError) Kind() VKErrorKind {
	switch apiError.Code {
	case 5:
		return VKErrorAuthentication
	case 14:
		return VKErrorCaptcha
	case 17:
		return VKErrorValidation
	case 25:
		return VKErrorManualAction
	default:
		return VKErrorGeneric
	}
}

type VKHTTPError struct {
	StatusCode int
}

func (httpError *VKHTTPError) Error() string {
	return fmt.Sprintf("vk HTTP status %d", httpError.StatusCode)
}

type VKTransportError struct {
	cause error
}

func (transportError *VKTransportError) Error() string {
	return "vk transport: " + transportError.cause.Error()
}

func (transportError *VKTransportError) Unwrap() error {
	return transportError.cause
}

func vkTransportError(err error) error {
	cause := errors.New("request failed")
	var networkError net.Error
	switch {
	case errors.Is(err, context.Canceled):
		cause = context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		cause = context.DeadlineExceeded
	case errors.As(err, &networkError) && networkError.Timeout():
		cause = context.DeadlineExceeded
	}
	return &VKTransportError{cause: cause}
}

type VKClient struct {
	token      string
	baseURL    string
	httpClient *http.Client
	rate       *vkRateGuard
}

type VKUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

type VKLongPollServer struct {
	Server string      `json:"server"`
	Key    string      `json:"key"`
	TS     json.Number `json:"ts"`
}

func NewVKClient(config Config, baseURL string, timeout time.Duration) (*VKClient, error) {
	if strings.TrimSpace(config.VKAccessToken) == "" {
		return nil, errors.New("vk access token is required")
	}
	if timeout <= 0 {
		return nil, errors.New("vk request timeout must be positive")
	}
	productionEndpoint := baseURL == ""
	if productionEndpoint {
		baseURL = "https://api.vk.com/method"
	}
	endpoint, err := url.Parse(baseURL)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("invalid vk API endpoint")
	}
	rate := newVKRateGuard()
	if !productionEndpoint {
		rate.minimum = 0
		rate.maxRetries = 0
	}
	return &VKClient{
		token:   config.VKAccessToken,
		baseURL: strings.TrimRight(baseURL, "/"),
		rate:    rate,
		httpClient: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (client *VKClient) ConfigureRateProtection(ctx context.Context, store vkRateStateStore, logger *slog.Logger) error {
	return client.rate.configure(ctx, store, logger)
}

func (client *VKClient) UsersGet(ctx context.Context, userIDs []int64) ([]VKUser, error) {
	identifiers := make([]string, len(userIDs))
	for index, identifier := range userIDs {
		identifiers[index] = strconv.FormatInt(identifier, 10)
	}
	var users []VKUser
	err := client.call(ctx, "users.get", url.Values{"user_ids": {strings.Join(identifiers, ",")}}, &users)
	if err != nil {
		return nil, err
	}
	return users, nil
}

func (client *VKClient) GetLongPollServer(ctx context.Context) (VKLongPollServer, error) {
	var server VKLongPollServer
	if err := client.call(ctx, "messages.getLongPollServer", url.Values{"need_ssl": {"1"}, "lp_version": {"3"}}, &server); err != nil {
		return VKLongPollServer{}, err
	}
	if server.Server == "" || server.Key == "" || server.TS == "" {
		return VKLongPollServer{}, ErrVKInvalidResponse
	}
	return server, nil
}

func (client *VKClient) call(ctx context.Context, method string, parameters url.Values, result any) error {
	return client.callVersion(ctx, method, parameters, result, VKAPIVersion)
}

func (client *VKClient) callVersion(ctx context.Context, method string, parameters url.Values, result any, version string) error {
	for attempt := 0; ; attempt++ {
		if err := client.rate.wait(ctx); err != nil {
			return err
		}
		err := client.callVersionOnce(ctx, method, parameters, result, version)
		if err == nil {
			return client.rate.successful(ctx)
		}
		var apiError *VKAPIError
		if errors.As(err, &apiError) && (apiError.Code == 9 || apiError.Code == 29) {
			return errors.Join(err, client.rate.activateFlood(ctx))
		}
		if retryErr := client.rate.retry(ctx, attempt, err); retryErr != nil {
			return retryErr
		}
	}
}

func (client *VKClient) callVersionOnce(ctx context.Context, method string, parameters url.Values, result any, version string) error {
	parameters.Set("access_token", client.token)
	parameters.Set("v", version)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.baseURL+"/"+method, strings.NewReader(parameters.Encode()))
	if err != nil {
		return errors.New("vk request creation failed")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return vkTransportError(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return &VKHTTPError{StatusCode: response.StatusCode}
	}
	const maxResponseBytes = 1 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return vkTransportError(err)
	}
	if len(body) > maxResponseBytes {
		return fmt.Errorf("%w: response too large", ErrVKInvalidResponse)
	}
	if !json.Valid(body) {
		return ErrVKInvalidJSON
	}
	var envelope struct {
		Response json.RawMessage `json:"response"`
		Error    *struct {
			Code int `json:"error_code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ErrVKInvalidResponse
	}
	if envelope.Error != nil {
		if envelope.Error.Code <= 0 || len(envelope.Response) != 0 {
			return ErrVKInvalidResponse
		}
		apiError := &VKAPIError{Code: envelope.Error.Code, Message: "request rejected"}
		switch apiError.Kind() {
		case VKErrorAuthentication:
			apiError.Message = "authentication failed"
		case VKErrorCaptcha:
			apiError.Message = "captcha required"
		case VKErrorValidation:
			apiError.Message = "validation required"
		case VKErrorManualAction:
			apiError.Message = "confirmation required"
		}
		return apiError
	}
	if len(envelope.Response) == 0 || string(envelope.Response) == "null" {
		return ErrVKInvalidResponse
	}
	if err := json.Unmarshal(envelope.Response, result); err != nil {
		return ErrVKInvalidResponse
	}
	return nil
}

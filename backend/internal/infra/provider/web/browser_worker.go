package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
)

const browserWorkerResponseLimit = 24 << 20
const browserWorkerRetryDelay = 250 * time.Millisecond

var browserWorkerClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

type browserWorkerRequest struct {
	BaseURL          string         `json:"baseURL"`
	Endpoint         string         `json:"endpoint"`
	ProxyURL         string         `json:"proxyURL,omitempty"`
	UserAgent        string         `json:"userAgent"`
	CloudflareCookie string         `json:"cloudflareCookies,omitempty"`
	SSOToken         string         `json:"ssoToken"`
	StatsigSignerURL string         `json:"statsigSignerURL"`
	RequestID        string         `json:"requestID"`
	TimeoutSeconds   int            `json:"timeoutSeconds"`
	Payload          map[string]any `json:"payload"`
}

type browserWorkerResponse struct {
	StatusCode int               `json:"statusCode"`
	Status     string            `json:"status"`
	Headers    map[string]string `json:"headers"`
	BodyBase64 string            `json:"bodyBase64"`
	Error      string            `json:"error"`
	Code       string            `json:"code"`
}

type browserWorkerWarmResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	Code  string `json:"code"`
}

type browserWorkerFailure struct {
	Code    string
	Message string
}

func (e *browserWorkerFailure) Error() string { return e.Message }

func (a *Adapter) openLiteImageUpstream(ctx context.Context, credential account.Credential, spec ModelSpec, prompt string) (*http.Response, *infraegress.Lease, string, error) {
	cfg := a.config()
	if cfg.BrowserWorkerURL == "" {
		response, lease, _, target, err := a.openChat(ctx, credential, "", spec, normalizedChatInput{Prompt: "Drawing: " + prompt})
		return response, lease, target, err
	}
	return a.openLiteImageWithBrowser(ctx, cfg, credential, spec, prompt)
}

func (a *Adapter) openLiteImageWithBrowser(ctx context.Context, cfg Config, credential account.Credential, spec ModelSpec, prompt string) (*http.Response, *infraegress.Lease, string, error) {
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return nil, nil, "", err
	}
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/rest/app-chat/conversations/new"
	var lease *infraegress.Lease
	var result browserWorkerResponse
	for egressAttempt := 0; egressAttempt < 2; egressAttempt++ {
		lease, err = a.egress.Acquire(ctx, domainegress.ScopeWeb, fmt.Sprintf("%d", credential.ID))
		if err != nil {
			return nil, nil, endpoint, err
		}
		value := browserWorkerRequest{
			BaseURL: cfg.BaseURL, Endpoint: endpoint, ProxyURL: lease.ProxyURL, UserAgent: lease.UserAgent,
			CloudflareCookie: lease.CFCookies, SSOToken: token, StatsigSignerURL: cfg.StatsigSignerURL,
			RequestID: newRequestUUID(), TimeoutSeconds: cfg.ImageTimeoutSeconds,
			Payload: buildWebChatPayload("Drawing: "+prompt, spec.Mode, nil),
		}
		requestCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.ImageTimeoutSeconds+30)*time.Second)
		result, err = callBrowserWorker(requestCtx, cfg.BrowserWorkerURL, value)
		cancel()
		if err == nil {
			break
		}
		var workerFailure *browserWorkerFailure
		if errors.As(err, &workerFailure) && workerFailure.Code == "proxy_unavailable" {
			a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, err)
			lease.Release()
			if egressAttempt == 0 {
				continue
			}
			return nil, nil, endpoint, &infraegress.UnavailableError{Scope: domainegress.ScopeWeb}
		}
		if errors.As(err, &workerFailure) && (workerFailure.Code == "browser_unavailable" || workerFailure.Code == "worker_unavailable") {
			lease.Release()
			return nil, nil, endpoint, &infraegress.UnavailableError{Scope: domainegress.ScopeWeb}
		}
		lease.Release()
		if looksLikeAntiBot([]byte(err.Error())) {
			a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, http.StatusForbidden, err)
			if egressAttempt == 0 {
				continue
			}
			return nil, nil, endpoint, fmt.Errorf("%w: %v", errWebAntiBot, err)
		}
		return nil, nil, endpoint, fmt.Errorf("Grok Web browser worker: %w", err)
	}
	if result.Error != "" {
		lease.Release()
		message := strings.TrimSpace(result.Error)
		if looksLikeAntiBot([]byte(message)) {
			return nil, nil, endpoint, fmt.Errorf("%w: %s", errWebAntiBot, message)
		}
		return nil, nil, endpoint, fmt.Errorf("Grok Web browser worker: %s", message)
	}
	if result.StatusCode < 100 || result.StatusCode > 599 {
		lease.Release()
		return nil, nil, endpoint, fmt.Errorf("Grok Web browser worker returned invalid upstream status")
	}
	upstreamBody, err := base64.StdEncoding.DecodeString(result.BodyBase64)
	if err != nil || len(upstreamBody) > browserWorkerResponseLimit {
		lease.Release()
		return nil, nil, endpoint, fmt.Errorf("Grok Web browser worker returned invalid upstream body")
	}
	headers := make(http.Header, len(result.Headers))
	for name, headerValue := range result.Headers {
		headers.Set(name, headerValue)
	}
	status := strings.TrimSpace(result.Status)
	if status == "" {
		status = fmt.Sprintf("%d %s", result.StatusCode, http.StatusText(result.StatusCode))
	}
	return &http.Response{
		StatusCode: result.StatusCode, Status: status, Header: headers,
		Body: io.NopCloser(bytes.NewReader(upstreamBody)), ContentLength: int64(len(upstreamBody)),
	}, lease, endpoint, nil
}

func callBrowserWorker(ctx context.Context, workerURL string, value browserWorkerRequest) (browserWorkerResponse, error) {
	return callBrowserWorkerAt(ctx, workerURL, "/v1/grok/fast-image", value)
}

func callBrowserWorkerQuota(ctx context.Context, workerURL string, value browserWorkerRequest) (browserWorkerResponse, error) {
	return callBrowserWorkerAt(ctx, workerURL, "/v1/grok/quota", value)
}

func callBrowserWorkerAt(ctx context.Context, workerURL, path string, value browserWorkerRequest) (browserWorkerResponse, error) {
	var result browserWorkerResponse
	err := callBrowserWorkerJSON(ctx, workerURL, path, value, &result)
	if isTransientBrowserWorkerFailure(err) {
		timer := time.NewTimer(browserWorkerRetryDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-timer.C:
		}
		result = browserWorkerResponse{}
		err = callBrowserWorkerJSON(ctx, workerURL, path, value, &result)
	}
	return result, err
}

func isTransientBrowserWorkerFailure(err error) bool {
	var failure *browserWorkerFailure
	if !errors.As(err, &failure) {
		return false
	}
	switch failure.Code {
	case "proxy_unavailable", "browser_unavailable", "worker_unavailable":
		return true
	default:
		return false
	}
}

func callBrowserWorkerWarm(ctx context.Context, workerURL string, value browserWorkerRequest) error {
	var result browserWorkerWarmResponse
	if err := callBrowserWorkerJSON(ctx, workerURL, "/v1/grok/warm", value, &result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("worker did not become ready")
	}
	return nil
}

func callBrowserWorkerJSON(ctx context.Context, workerURL, path string, value browserWorkerRequest, result any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(workerURL, "/")+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := browserWorkerClient.Do(request)
	if err != nil {
		return &browserWorkerFailure{Code: "worker_unavailable", Message: "browser worker unavailable"}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, browserWorkerResponseLimit+1))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if len(body) > browserWorkerResponseLimit {
		return fmt.Errorf("response exceeds 24 MiB")
	}
	if json.Unmarshal(body, result) != nil {
		return fmt.Errorf("invalid JSON response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure browserWorkerWarmResponse
		if json.Unmarshal(body, &failure) == nil && strings.TrimSpace(failure.Error) != "" {
			return &browserWorkerFailure{Code: strings.TrimSpace(failure.Code), Message: strings.TrimSpace(failure.Error)}
		}
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return nil
}

// Package model provider factory and shared HTTP helpers.
package model

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultOpenAIBaseURL    = "https://api.openai.com/v1"
	defaultDeepSeekBaseURL  = "https://api.deepseek.com/v1"
	defaultAnthropicBaseURL = "https://api.anthropic.com/v1"
	defaultOllamaBaseURL    = "http://localhost:11434"

	defaultOpenAIModel    = "gpt-4o-mini"
	defaultDeepSeekModel  = "deepseek-chat"
	defaultAnthropicModel = "claude-sonnet-4-5"
	defaultOllamaModel    = "llama3.1"

	maxAttempts    = 3
	errBodyExcerpt = 500
	// maxResponseBytes caps a non-streaming response body. A body
	// larger than this is reported as a truncation error instead of
	// being silently cut and mis-decoded as JSON.
	maxResponseBytes = 4 << 20
	// maxRetryAfter caps a server-supplied Retry-After delay so a
	// malicious or confused server cannot stall a call for minutes.
	maxRetryAfter = 30 * time.Second
)

var backoffDelays = []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}

// sharedHTTPClient is used by all providers. The 120s timeout bounds any
// single attempt; per-request cancellation still comes from ctx.
var sharedHTTPClient = &http.Client{Timeout: 120 * time.Second}

// NewProvider builds the provider named by cfg.Name. The core never
// hardcodes a provider; selection happens only here, from config.
func NewProvider(cfg ProviderConfig) (Provider, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Name)) {
	case "openai":
		if strings.TrimSpace(cfg.APIKey) == "" {
			return nil, fmt.Errorf("openai: API key is required (set ProviderConfig.APIKey)")
		}
		if strings.TrimSpace(cfg.BaseURL) == "" {
			cfg.BaseURL = defaultOpenAIBaseURL
		}
		if strings.TrimSpace(cfg.Model) == "" {
			cfg.Model = defaultOpenAIModel
		}
		return &openAIProvider{cfg: cfg, client: sharedHTTPClient}, nil
	case "deepseek":
		// DeepSeek's API is OpenAI-compatible: same /chat/completions
		// wire format, reached at its own base URL. It reuses the
		// OpenAI-compatible client with DeepSeek's name so errors and
		// Name() are labelled correctly.
		if strings.TrimSpace(cfg.APIKey) == "" {
			return nil, fmt.Errorf("deepseek: API key is required (set ProviderConfig.APIKey)")
		}
		if strings.TrimSpace(cfg.BaseURL) == "" {
			cfg.BaseURL = defaultDeepSeekBaseURL
		}
		if strings.TrimSpace(cfg.Model) == "" {
			cfg.Model = defaultDeepSeekModel
		}
		return &openAIProvider{name: "deepseek", cfg: cfg, client: sharedHTTPClient}, nil
	case "anthropic":
		if strings.TrimSpace(cfg.APIKey) == "" {
			return nil, fmt.Errorf("anthropic: API key is required (set ProviderConfig.APIKey)")
		}
		if strings.TrimSpace(cfg.BaseURL) == "" {
			cfg.BaseURL = defaultAnthropicBaseURL
		}
		if strings.TrimSpace(cfg.Model) == "" {
			cfg.Model = defaultAnthropicModel
		}
		return &anthropicProvider{cfg: cfg, client: sharedHTTPClient}, nil
	case "ollama":
		// Ollama is local and needs no API key.
		if strings.TrimSpace(cfg.BaseURL) == "" {
			cfg.BaseURL = defaultOllamaBaseURL
		}
		if strings.TrimSpace(cfg.Model) == "" {
			cfg.Model = defaultOllamaModel
		}
		return &ollamaProvider{cfg: cfg, client: sharedHTTPClient}, nil
	default:
		return nil, fmt.Errorf("unknown provider %q: supported providers are openai, deepseek, anthropic, ollama", cfg.Name)
	}
}

// isRetryableStatus reports whether a status code is worth retrying.
func isRetryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// sleepBackoff waits d unless ctx is done first.
func sleepBackoff(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// retryDelay returns how long to wait before the next attempt after a
// retryable response: the server's Retry-After header when present and
// parseable (seconds, or an HTTP date), capped at maxRetryAfter, else
// the exponential backoff for this attempt.
func retryDelay(resp *http.Response, attempt int) time.Duration {
	if resp != nil {
		if v := strings.TrimSpace(resp.Header.Get("Retry-After")); v != "" {
			if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
				d := time.Duration(secs) * time.Second
				if d > maxRetryAfter {
					d = maxRetryAfter
				}
				return d
			}
			if t, err := http.ParseTime(v); err == nil {
				d := time.Until(t)
				if d < 0 {
					d = 0
				}
				if d > maxRetryAfter {
					d = maxRetryAfter
				}
				return d
			}
		}
	}
	if attempt < len(backoffDelays) {
		return backoffDelays[attempt]
	}
	return backoffDelays[len(backoffDelays)-1]
}

// isRetryableTransportError reports whether a failed HTTP round trip is
// worth retrying. Transient faults (timeouts, connection resets, an
// unexpected EOF) may self-heal; configuration faults never do, so DNS
// failures and TLS/certificate errors are not retried — retrying them
// only burns the attempt budget and delays a clear error.
func isRetryableTransportError(err error) bool {
	if err == nil {
		return false
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Unwrap()
		if err == nil {
			return false
		}
	}
	// DNS and TLS failures are configuration errors, not transient.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return false
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return false
	}
	var recordErr tls.RecordHeaderError
	if errors.As(err, &recordErr) {
		return false
	}
	var x509Err x509.CertificateInvalidError
	if errors.As(err, &x509Err) {
		return false
	}
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return false
	}
	var hostnameErr x509.HostnameError
	if errors.As(err, &hostnameErr) {
		return false
	}
	// Timeouts and reset/aborted connections may self-heal.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNABORTED) {
		return true
	}
	return false
}

// readCappedBody reads resp.Body up to maxResponseBytes. A body larger
// than the cap is reported as an explicit truncation error instead of
// being silently cut (a cut body then fails JSON decoding with a
// misleading syntax error).
func readCappedBody(resp *http.Response, url string) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("reading response from %s: %w", url, err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("response from %s exceeded the %d byte limit and was truncated", url, maxResponseBytes)
	}
	return body, nil
}

// doWithRetry performs one HTTP request with up to maxAttempts attempts,
// retrying only on 429/5xx responses and retryable transport errors,
// with exponential backoff (100ms, 200ms, 400ms) that honours the
// server's Retry-After header on 429/503 and ctx cancellation. The
// request body is re-sent on every attempt. It returns the final status
// code and body even for non-2xx responses; callers decide how to map
// those to errors. A non-nil error means no usable response was obtained.
func doWithRetry(ctx context.Context, client *http.Client, method, url string, headers map[string]string, body []byte) (int, []byte, error) {
	if client == nil {
		client = sharedHTTPClient
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		if err != nil {
			return 0, nil, fmt.Errorf("building request to %s: %w", url, err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("request to %s failed (attempt %d/%d): %w", url, attempt+1, maxAttempts, err)
			if ctx.Err() != nil {
				return 0, nil, ctx.Err()
			}
			if attempt < maxAttempts-1 && isRetryableTransportError(err) {
				if berr := sleepBackoff(ctx, backoffDelays[attempt]); berr != nil {
					return 0, nil, berr
				}
				continue
			}
			return 0, nil, lastErr
		}
		respBody, readErr := readCappedBody(resp, url)
		if readErr != nil {
			return 0, nil, readErr
		}
		if isRetryableStatus(resp.StatusCode) && attempt < maxAttempts-1 {
			lastErr = fmt.Errorf("request to %s returned retryable status %d", url, resp.StatusCode)
			if berr := sleepBackoff(ctx, retryDelay(resp, attempt)); berr != nil {
				return 0, nil, berr
			}
			continue
		}
		return resp.StatusCode, respBody, nil
	}
	if lastErr != nil {
		return 0, nil, lastErr
	}
	return 0, nil, fmt.Errorf("request to %s failed after %d attempts", url, maxAttempts)
}

// doStreamRequest performs the setup request for a streaming call,
// retrying the setup (not mid-stream chunks) on 429/5xx responses —
// honouring Retry-After — and on retryable transport errors, with the
// same backoff as doWithRetry. Once a 2xx response is returned the
// caller owns resp.Body and no further retries happen: a stream that
// fails part-way through is reported, never silently restarted. A
// non-nil error means no usable response was obtained; a non-2xx final
// response is returned as resp (nil error) so the caller can report
// the provider's error body.
func doStreamRequest(ctx context.Context, client *http.Client, method, url string, headers map[string]string, body []byte) (*http.Response, error) {
	if client == nil {
		client = sharedHTTPClient
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("building request to %s: %w", url, err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("request to %s failed (attempt %d/%d): %w", url, attempt+1, maxAttempts, err)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if attempt < maxAttempts-1 && isRetryableTransportError(err) {
				if berr := sleepBackoff(ctx, backoffDelays[attempt]); berr != nil {
					return nil, berr
				}
				continue
			}
			return nil, lastErr
		}
		if isRetryableStatus(resp.StatusCode) && attempt < maxAttempts-1 {
			lastErr = fmt.Errorf("request to %s returned retryable status %d", url, resp.StatusCode)
			delay := retryDelay(resp, attempt)
			resp.Body.Close()
			if berr := sleepBackoff(ctx, delay); berr != nil {
				return nil, berr
			}
			continue
		}
		return resp, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("request to %s failed after %d attempts", url, maxAttempts)
}

// estimateUsage fills in only the usage fields the provider left at
// zero, from a rough token estimate, and derives the total from the
// (possibly estimated) components. A reported non-zero field is never
// overwritten: a provider that reports prompt tokens but no total
// keeps its prompt count.
func estimateUsage(req ChatRequest, msg Message, reported Usage) Usage {
	if reported.PromptTokens == 0 {
		reported.PromptTokens = CountTokens(joinContents(req.Messages))
	}
	if reported.CompletionTokens == 0 {
		reported.CompletionTokens = CountTokens(msg.Content)
	}
	if reported.TotalTokens == 0 {
		reported.TotalTokens = reported.PromptTokens + reported.CompletionTokens
	}
	return reported
}

// apiError builds a descriptive error for a non-2xx response, including a
// capped excerpt of the body so provider error messages stay visible
// without flooding logs.
func apiError(provider string, status int, body []byte) error {
	excerpt := strings.TrimSpace(string(body))
	if len(excerpt) > errBodyExcerpt {
		excerpt = excerpt[:errBodyExcerpt] + "..."
	}
	if excerpt == "" {
		return fmt.Errorf("%s: request failed with status %d (empty response body)", provider, status)
	}
	return fmt.Errorf("%s: request failed with status %d: %s", provider, status, excerpt)
}

// bodyExcerpt is a small helper for stream error paths.
func bodyExcerpt(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > errBodyExcerpt {
		s = s[:errBodyExcerpt] + "..."
	}
	return s
}

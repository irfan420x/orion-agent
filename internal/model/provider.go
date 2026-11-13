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
	"sync"
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

// --- Timeout design (Audit 2 §6) ---------------------------------------
//
// One stalled attempt used to eat the whole agent step: the per-attempt
// HTTP client timeout (120s) was the same as the step context (120s),
// so a hung DeepSeek call left the agent with nothing — no time to
// retry, no time to report a clear error. The three timeouts below
// are DISTINCT, one per failure mode, and they nest:
//
//		connectTimeout  <  progressTimeout  <  totalTimeout  <  step timeout (120s)
//
//	  - connectTimeout (dial / TLS handshake) fails fast when the
//	    endpoint is unreachable or black-holed: a TCP connect or TLS
//	    handshake that normally takes well under a second gets 10s,
//	    which is generous for a cold route and still an order of
//	    magnitude faster than the old 120s stall.
//	  - progressTimeout (first byte AND mid-stream silence) is the key
//	    mechanism. It is a PROGRESS timeout, not a wall-clock one: any
//	    byte that arrives resets the window, so a reasoning model that
//	    streams slowly but STEADILY is never killed, however long the
//	    whole response takes. Only a stream that goes completely silent
//	    for the full window is aborted. 60s is deliberately generous:
//	    reasoning models (DeepSeek reasoner, Claude extended thinking)
//	    can pause for tens of seconds between bursts — under load,
//	    time-to-first-token alone can reach ~30s — and killing a live
//	    turn for a 30s pause would be worse than waiting. A 60s
//	    silence, by contrast, means the server or a proxy is stuck,
//	    not thinking. It is enforced twice: the transport's
//	    ResponseHeaderTimeout bounds the wait for the first byte
//	    (headers), and progressReader bounds the wait between body
//	    reads (chunks).
//	  - totalTimeout is the outer bound for one whole Chat or Stream
//	    invocation, INCLUDING its internal retries, backoff and any
//	    Chat fallback. 100s sits inside the agent step's 120s
//	    (internal/core: defaultTimeoutSeconds) with 20s to spare, so
//	    the provider returns a clear timeout error while the step
//	    still has time to record it and reply, instead of the step
//	    context killing the call mid-flight with a bare
//	    context.DeadlineExceeded. If the step budget ever grows,
//	    totalTimeout should grow with it; it must always stay below
//	    the step timeout. The caller's own context remains the final
//	    outer bound: a shorter caller deadline always wins.
//
// The values are package-level vars (not consts) so tests can tune
// them against fake slow servers without waiting out production
// values. They are read at call time, except for the transport
// fields, which are fixed when the client is built — a test that
// changes connectTimeout or progressTimeout must rebuild the client
// with newSharedHTTPClient() (see setTestTimeouts in the tests).
// Production code never changes them.
var (
	connectTimeout  = 10 * time.Second
	progressTimeout = 60 * time.Second
	totalTimeout    = 100 * time.Second
)

// newSharedHTTPClient builds the HTTP client shared by all providers
// from the current timeout vars. The transport bounds the dial and
// TLS handshake with connectTimeout and the wait for response headers
// with progressTimeout (the first-byte half of the progress timeout;
// the body half is progressReader, applied per response). There is
// deliberately NO http.Client.Timeout here: the per-call deadline is
// totalTimeout, applied as a context by doWithRetry/doStreamRequest
// (and by Stream for the whole streaming call) so it is read at call
// time, covers retries and backoff, and stays tunable in tests — a
// Client.Timeout is fixed when the client is built and would be a
// second, per-attempt wall-clock bound that a slow-but-steady stream
// must never hit.
func newSharedHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			// ProxyFromEnvironment is load-bearing: this VM reaches
			// the internet only through an egress proxy
			// (HTTPS_PROXY). A custom Transport with Proxy == nil
			// means NO proxy, so direct connections fail with
			// "tls: first record does not look like a TLS
			// handshake" — the old DefaultTransport was
			// proxy-aware and this must stay that way.
			Proxy: http.ProxyFromEnvironment,
			// The dial timeout is read from the var on every dial,
			// so a tuned connectTimeout takes effect immediately.
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				d := &net.Dialer{Timeout: connectTimeout}
				return d.DialContext(ctx, network, address)
			},
			TLSHandshakeTimeout:   connectTimeout,
			ResponseHeaderTimeout: progressTimeout,
		},
	}
}

// sharedHTTPClient is used by all providers.
var sharedHTTPClient = newSharedHTTPClient()

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

// progressTimeoutError is returned by progressReader when a response
// body goes silent for progressTimeout. It implements net.Error with
// Timeout() == true, so the existing retry policy — which retries
// timeouts and treats them as transient — applies to a stalled body
// without any special-casing, and so callers can recognise it with
// errors.As / isProgressTimeout.
type progressTimeoutError struct {
	// timeout is the silence window that elapsed with no bytes.
	timeout time.Duration
}

func (e *progressTimeoutError) Error() string {
	return fmt.Sprintf("no response data for %s (progress timeout: the provider stopped sending bytes)", e.timeout)
}

// Timeout reports true: a stalled provider is a timeout, not a
// permanent failure.
func (e *progressTimeoutError) Timeout() bool { return true }

// Temporary reports true: the stall may clear on retry.
func (e *progressTimeoutError) Temporary() bool { return true }

// isProgressTimeout reports whether err (or anything it wraps) is a
// progress timeout.
func isProgressTimeout(err error) bool {
	var pte *progressTimeoutError
	return errors.As(err, &pte)
}

// readResult is one chunk (or terminal error) delivered by the
// progressReader pump to the consuming Read.
type readResult struct {
	data []byte
	err  error
}

// progressReader wraps a response body and aborts reads that go silent
// for the configured window: if no bytes arrive within timeout, Read
// returns a *progressTimeoutError and the body is closed.
//
// Progress, not wall-clock, is the signal: every byte that arrives
// resets the window, so a reasoning model that streams slowly but
// steadily is never killed however long the whole response takes —
// only a stream that goes completely silent for the full window is
// aborted. The total wall-clock bound is the call context's
// totalTimeout, applied separately by the shared helpers.
//
// One pump goroutine runs per body. The pump owns its read buffer and
// hands completed chunks to Read over a channel, so no buffer is ever
// shared with a caller that may have moved on after a timeout. Close
// unblocks the pump; the pump also exits on its own once the
// underlying body reports an error or EOF, so a fully-read body
// leaves no goroutine behind.
type progressReader struct {
	rc      io.ReadCloser
	timeout time.Duration

	ch   chan readResult
	done chan struct{}

	// buf holds bytes already received but not yet consumed because
	// the caller's Read buffer was smaller than the chunk. pendingErr
	// holds a terminal error that arrived with a final chunk: the
	// bytes are delivered first, the error on the next Read, per the
	// io.Reader contract. Both are touched only by the consuming
	// goroutine.
	buf        []byte
	pendingErr error

	closeOnce sync.Once
}

// newProgressReader wraps rc so reads abort after timeout of silence.
// The timeout is read from progressTimeout at wrap time.
func newProgressReader(rc io.ReadCloser) io.ReadCloser {
	pr := &progressReader{
		rc:      rc,
		timeout: progressTimeout,
		ch:      make(chan readResult),
		done:    make(chan struct{}),
	}
	go pr.pump()
	return pr
}

// pump reads the underlying body and forwards each chunk. It exits
// when the body ends/errors, or when Close signals done.
func (pr *progressReader) pump() {
	buf := make([]byte, 32*1024)
	for {
		n, err := pr.rc.Read(buf)
		var data []byte
		if n > 0 {
			data = make([]byte, n)
			copy(data, buf[:n])
		}
		select {
		case pr.ch <- readResult{data: data, err: err}:
		case <-pr.done:
			return
		}
		if err != nil {
			return
		}
	}
}

// Read returns bytes already received, or waits for the next chunk
// until the progress window elapses. A (0, nil) result from the body
// is not progress: the wait continues on the SAME timer rather than
// starting a fresh window, so a body that returns empty reads forever
// still trips the timeout.
func (pr *progressReader) Read(p []byte) (int, error) {
	if len(pr.buf) > 0 {
		n := copy(p, pr.buf)
		pr.buf = pr.buf[n:]
		return n, nil
	}
	if pr.pendingErr != nil {
		err := pr.pendingErr
		pr.pendingErr = nil
		return 0, err
	}
	timer := time.NewTimer(pr.timeout)
	defer timer.Stop()
	for {
		select {
		case res := <-pr.ch:
			if len(res.data) > 0 {
				n := copy(p, res.data)
				if n < len(res.data) {
					pr.buf = res.data[n:]
				}
				if res.err != nil {
					pr.pendingErr = res.err
				}
				return n, nil
			}
			if res.err != nil {
				return 0, res.err
			}
			// Empty read, no error, no progress: keep waiting
			// on the same timer.
			continue
		case <-timer.C:
			err := &progressTimeoutError{timeout: pr.timeout}
			pr.pendingErr = err
			// Closing the body unblocks the pump's Read so the
			// goroutine exits instead of leaking.
			pr.Close()
			return 0, err
		}
	}
}

// Close closes the underlying body and signals the pump to exit. It
// is idempotent: the body may be closed by the reader, the helpers,
// and the provider's stream loop on different paths.
func (pr *progressReader) Close() error {
	pr.closeOnce.Do(func() { close(pr.done) })
	return pr.rc.Close()
}

// readCappedBody reads resp.Body up to maxResponseBytes. A body larger
// than the cap is reported as an explicit truncation error instead of
// being silently cut (a cut body then fails JSON decoding with a
// misleading syntax error). The body is read through a progressReader,
// so a server that sends headers and then goes silent aborts with a
// progress timeout instead of hanging until the caller's deadline.
func readCappedBody(resp *http.Response, url string) ([]byte, error) {
	pr := newProgressReader(resp.Body)
	body, err := io.ReadAll(io.LimitReader(pr, maxResponseBytes+1))
	pr.Close()
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
//
// The whole call — every attempt, backoff and body read — is bounded
// by totalTimeout via a derived context, so retries can never push the
// call past the agent step's budget. A body that stalls mid-read
// aborts with a progress timeout, which is retryable like any other
// transient transport fault; a body that exceeds the size cap is not
// retried, since re-issuing the request would return the same
// oversized body.
func doWithRetry(ctx context.Context, client *http.Client, method, url string, headers map[string]string, body []byte) (int, []byte, error) {
	if client == nil {
		client = sharedHTTPClient
	}
	ctx, cancel := context.WithTimeout(ctx, totalTimeout)
	defer cancel()
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
			// A stalled or reset body is transient: retry the
			// request like a failed round trip. A truncation
			// error is not retried — the same oversized body
			// would come back.
			if attempt < maxAttempts-1 && (isProgressTimeout(readErr) || isRetryableTransportError(readErr)) {
				lastErr = fmt.Errorf("request to %s failed reading the response (attempt %d/%d): %w", url, attempt+1, maxAttempts, readErr)
				if berr := sleepBackoff(ctx, backoffDelays[attempt]); berr != nil {
					return 0, nil, berr
				}
				continue
			}
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
//
// The returned body is wrapped in a progressReader, so a stream that
// goes silent mid-way aborts the read with a progress timeout instead
// of hanging: the first-byte wait is bounded by the transport's
// ResponseHeaderTimeout, the wait between chunks by the reader.
// Callers close resp.Body as usual — closing the wrapper closes the
// underlying body and stops its pump goroutine.
//
// NOTE: unlike doWithRetry, this helper does NOT derive a totalTimeout
// context: the response body outlives the call, and cancelling a
// context derived here would kill the caller's stream the moment
// this function returned. The total bound for a streaming call is
// owned by the Stream functions, which derive one context with
// totalTimeout for the whole call (setup attempts, scan, fallback)
// and pass it in.
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
		// Wrap the body so every read — the stream scan and any
		// error-body read — aborts on a progress timeout.
		resp.Body = newProgressReader(resp.Body)
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

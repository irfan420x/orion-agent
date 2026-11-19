// Provider-failure classification for the fallback chain.
//
// A failed turn continues on the next provider in the chain only when
// the failure is one the next provider can plausibly survive: auth,
// billing, rate-limit, a missing model, a 5xx, or a retryable transport
// fault. A bad request (400) is the caller's fault — another provider
// would reject the same prompt — so it never fails over, and unknown
// errors fail closed (no switch) rather than burning a call on a
// provider that cannot help.
package model

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestAPIErrorMessageParity(t *testing.T) {
	// apiError's text is the contract other layers match on (and
	// what operators see in logs): the typed error must render
	// byte-identically to the old plain fmt.Errorf text.
	cases := []struct {
		provider string
		status   int
		body     string
		want     string
	}{
		{"deepseek", 402, `{"error":{"message":"Insufficient Balance"}}`,
			`deepseek: request failed with status 402: {"error":{"message":"Insufficient Balance"}}`},
		{"openai", 401, "", "openai: request failed with status 401 (empty response body)"},
		{"openai", 401, "   ", "openai: request failed with status 401 (empty response body)"},
		{"anthropic", 429, "rate limited", "anthropic: request failed with status 429: rate limited"},
	}
	for _, tc := range cases {
		err := apiError(tc.provider, tc.status, []byte(tc.body))
		if err.Error() != tc.want {
			t.Errorf("apiError(%q, %d, %q) = %q, want %q", tc.provider, tc.status, tc.body, err.Error(), tc.want)
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Errorf("apiError(%q, %d, ...) is not an *APIError: %T", tc.provider, tc.status, err)
			continue
		}
		if apiErr.Provider != tc.provider || apiErr.Status != tc.status {
			t.Errorf("APIError fields = %+v, want provider %q status %d", apiErr, tc.provider, tc.status)
		}
	}
	// A body over the excerpt cap is truncated with "..." (the old
	// behaviour) and still classifies by status.
	long := strings.Repeat("x", 600)
	err := apiError("deepseek", 500, []byte(long))
	if !strings.HasSuffix(err.Error(), "...") || !strings.Contains(err.Error(), "status 500") {
		t.Errorf("long-body apiError = %q, want truncation + status 500", err.Error())
	}
	if ClassifyProviderError(err) != FailoverTransient {
		t.Errorf("long-body 500 classifies as %v, want FailoverTransient", ClassifyProviderError(err))
	}
}

func TestClassifyProviderErrorStatuses(t *testing.T) {
	cases := []struct {
		status int
		want   FailoverClass
	}{
		{400, FailoverNone},
		{401, FailoverAuth},
		{402, FailoverBilling},
		{403, FailoverAuth},
		{404, FailoverModelNotFound},
		{409, FailoverNone},
		{422, FailoverNone},
		{429, FailoverRateLimit},
		{500, FailoverTransient},
		{502, FailoverTransient},
		{503, FailoverTransient},
	}
	for _, tc := range cases {
		err := apiError("deepseek", tc.status, []byte("boom"))
		if got := ClassifyProviderError(err); got != tc.want {
			t.Errorf("status %d: Classify = %v, want %v", tc.status, got, tc.want)
		}
		// Classification survives wrapping (the agent wraps
		// provider errors with step context).
		wrapped := fmt.Errorf("core: provider call at step 1: %w", err)
		if got := ClassifyProviderError(wrapped); got != tc.want {
			t.Errorf("status %d wrapped: Classify = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestClassifyProviderErrorTransport(t *testing.T) {
	// Timeouts and reset connections are transient: another
	// provider (another host) may answer.
	timeoutErr := &net.DNSError{IsTimeout: true, Name: "api.deepseek.com"}
	if got := ClassifyProviderError(timeoutErr); got != FailoverTransient {
		t.Errorf("timeout DNS error: Classify = %v, want FailoverTransient", got)
	}
	resetErr := fmt.Errorf("request failed: %w", syscall.ECONNRESET)
	if got := ClassifyProviderError(resetErr); got != FailoverTransient {
		t.Errorf("connection reset: Classify = %v, want FailoverTransient", got)
	}
	// DNS and TLS failures are configuration faults, not
	// failover triggers: retrying them on another provider only
	// burns the attempt budget and delays a clear error.
	dnsErr := &net.DNSError{IsNotFound: true, Name: "api.deepseek.com"}
	if got := ClassifyProviderError(dnsErr); got != FailoverNone {
		t.Errorf("DNS not-found: Classify = %v, want FailoverNone", got)
	}
	certErr := &tls.CertificateVerificationError{Err: errors.New("unknown authority")}
	if got := ClassifyProviderError(certErr); got != FailoverNone {
		t.Errorf("TLS cert error: Classify = %v, want FailoverNone", got)
	}
	// Unknown errors fail closed: no switch on a failure the
	// classifier cannot understand.
	if got := ClassifyProviderError(errors.New("something odd happened")); got != FailoverNone {
		t.Errorf("unknown error: Classify = %v, want FailoverNone", got)
	}
	if got := ClassifyProviderError(nil); got != FailoverNone {
		t.Errorf("nil error: Classify = %v, want FailoverNone", got)
	}
}

func TestFailoverCooldowns(t *testing.T) {
	// The cooldown table is the anti-hammering rule: a provider
	// that just failed is not retried until its cooldown expires,
	// so a dead provider cannot eat every turn's first attempt.
	// Billing benches the longest (the balance will not refill
	// mid-turn); a transient blip is retried quickly.
	cases := []struct {
		class FailoverClass
		want  time.Duration
	}{
		{FailoverNone, 0},
		{FailoverAuth, 5 * time.Minute},
		{FailoverBilling, 60 * time.Minute},
		{FailoverRateLimit, 60 * time.Minute},
		{FailoverTransient, 60 * time.Second},
		{FailoverModelNotFound, 60 * time.Minute},
	}
	for _, tc := range cases {
		if got := tc.class.Cooldown(); got != tc.want {
			t.Errorf("%v cooldown = %v, want %v", tc.class, got, tc.want)
		}
	}
	for _, c := range []FailoverClass{FailoverAuth, FailoverBilling, FailoverRateLimit, FailoverTransient, FailoverModelNotFound} {
		if c.String() == "" || c.String() == "unknown" {
			t.Errorf("class %d has no name", int(c))
		}
	}
}

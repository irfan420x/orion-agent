// Provider-failure classification for the fallback chain.
//
// A failed turn continues on the next provider in the chain only when
// the failure is one the next provider can plausibly survive. The
// classifier is the single place that decides that: the agent loop
// asks it about every provider error, and the chain benches the
// failed provider for the class's cooldown (see Cooldown).
//
// Classes (Hermes' error_classifier.py maps the same failures, but
// Orion classifies from its own typed APIError instead of string
// matching, and benches per provider rather than per credential
// pool — Orion has one key per provider, so the pool distinction
// does not exist here):
//
//   - Auth (401/403): the key is wrong or revoked. The next
//     provider has its own key, so fail over; bench 5 minutes.
//   - Billing (402): the balance is exhausted. Never retried on the
//     same provider (a retry would fail identically and burn the
//     attempt budget); fail over at once; bench 60 minutes.
//   - RateLimit (429): the provider is throttling. The provider
//     layer already retried internally with backoff; fail over;
//     bench 60 minutes.
//   - Transient (5xx, timeouts, reset connections): the endpoint
//     is sick. Another host may answer; bench 60 seconds only.
//   - ModelNotFound (404): the model does not exist on this
//     provider. The next provider runs its own model; bench
//     60 minutes (the model will not appear mid-session).
//   - None (400, other 4xx, DNS/TLS config faults, unknown errors):
//     fail closed — no switch. A 400 is the caller's fault and
//     another provider would reject the same request; DNS/TLS
//     faults are configuration errors; an unrecognised error must
//     never trigger a switch on a guess.
package model

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"time"
)

// APIError is a provider's non-2xx response, typed so the failover
// classifier can read the status without string matching. Its Error
// text is byte-identical to the old plain apiError text (the
// contract operators and logs match on), including the 500-byte
// body excerpt and the empty-body form.
type APIError struct {
	Provider string
	Status   int
	Body     string // excerpt of the response body (capped)
}

func (e *APIError) Error() string {
	if e == nil {
		return "<nil api error>"
	}
	if e.Body == "" {
		return fmt.Sprintf("%s: request failed with status %d (empty response body)", e.Provider, e.Status)
	}
	return fmt.Sprintf("%s: request failed with status %d: %s", e.Provider, e.Status, e.Body)
}

// FailoverClass is one failure kind the classifier recognises.
type FailoverClass int

const (
	// FailoverNone: do not switch providers for this failure.
	FailoverNone FailoverClass = iota
	// FailoverAuth: 401/403 — bad or revoked key.
	FailoverAuth
	// FailoverBilling: 402 — balance/quota exhausted.
	FailoverBilling
	// FailoverRateLimit: 429 — throttled.
	FailoverRateLimit
	// FailoverTransient: 5xx, timeouts, reset connections.
	FailoverTransient
	// FailoverModelNotFound: 404 — model not on this provider.
	FailoverModelNotFound
)

func (c FailoverClass) String() string {
	switch c {
	case FailoverNone:
		return "none"
	case FailoverAuth:
		return "auth"
	case FailoverBilling:
		return "billing"
	case FailoverRateLimit:
		return "rate-limit"
	case FailoverTransient:
		return "transient"
	case FailoverModelNotFound:
		return "model-not-found"
	default:
		return "unknown"
	}
}

// Cooldown is how long a provider that failed with this class stays
// benched (skipped by chain selection) before it may be tried again.
// None benches nothing: the failure was not recognised as a
// failover trigger, so there is nothing to bench.
func (c FailoverClass) Cooldown() time.Duration {
	switch c {
	case FailoverAuth:
		return 5 * time.Minute
	case FailoverBilling, FailoverRateLimit, FailoverModelNotFound:
		return 60 * time.Minute
	case FailoverTransient:
		return 60 * time.Second
	default:
		return 0
	}
}

// ClassifyProviderError maps a provider-call error to its failover
// class. It unwraps (the agent wraps provider errors with step
// context) and never panics on a nil error.
func ClassifyProviderError(err error) FailoverClass {
	if err == nil {
		return FailoverNone
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.Status == 401 || apiErr.Status == 403:
			return FailoverAuth
		case apiErr.Status == 402:
			return FailoverBilling
		case apiErr.Status == 429:
			return FailoverRateLimit
		case apiErr.Status == 404:
			return FailoverModelNotFound
		case apiErr.Status >= 500:
			return FailoverTransient
		default:
			return FailoverNone
		}
	}
	// Transport faults: unwrap url.Error the way the retry policy
	// does, then apply the same config-fault rule it uses (DNS and
	// TLS failures are configuration errors, never retried — and
	// never failed over either). A timeout is checked FIRST: a DNS
	// lookup that times out is a transient resolver stall, not a
	// configuration fault, and a slow provider is exactly what
	// the next provider in the chain is for.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return FailoverTransient
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return FailoverNone
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return FailoverNone
	}
	var recordErr tls.RecordHeaderError
	if errors.As(err, &recordErr) {
		return FailoverNone
	}
	var x509Err x509.CertificateInvalidError
	if errors.As(err, &x509Err) {
		return FailoverNone
	}
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return FailoverNone
	}
	var hostnameErr x509.HostnameError
	if errors.As(err, &hostnameErr) {
		return FailoverNone
	}
	// Timeouts and reset/aborted connections may self-heal on
	// another host. (Timeouts were already handled above; the
	// EOF/reset cases below are the non-timeout half.)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return FailoverTransient
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNABORTED) {
		return FailoverTransient
	}
	return FailoverNone
}

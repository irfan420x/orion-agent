// Package security — secret scanner.
//
// RedactSecrets replaces detected secrets with "[REDACTED]"; ScanSecrets
// reports one Finding per distinct secret value found. The scanner never
// executes anything and never logs or stores the secret values it finds —
// findings carry only the secret type and the literal "[REDACTED]" marker.
package security

import (
	"regexp"
	"strings"
	"unicode"
)

// Redacted is the placeholder substituted for every detected secret.
const Redacted = "[REDACTED]"

// Finding describes one distinct secret value found by ScanSecrets.
// Redacted is always the literal "[REDACTED]" marker — the real secret
// value is never retained.
type Finding struct {
	Type     string
	Redacted string
}

var (
	// Private-key blocks first: they can span lines and may contain
	// material that other patterns would match inside them.
	privateKeyRe = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)

	// Anthropic keys must be matched before OpenAI keys: "sk-ant-..."
	// would otherwise also match the generic "sk-" pattern.
	anthropicRe = regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{8,}`)
	// OpenAI keys include hyphens/underscores after the prefix:
	// "sk-proj-...", "sk-svcacct-...", "sk-test...". The character class
	// must therefore admit "-" and "_", not just alphanumerics.
	openAIRe = regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`)
	stripeRe = regexp.MustCompile(`(?i)(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{8,}`)
	slackRe  = regexp.MustCompile(`xox[baprse]-[A-Za-z0-9-]{10,}`)
	// Slack incoming-webhook URLs: the whole path after /services/
	// is the secret.
	slackWebhookRe = regexp.MustCompile(`(https://hooks\.slack\.com/services/)[A-Za-z0-9_-]+/[A-Za-z0-9_-]+/[A-Za-z0-9_-]{16,}`)
	githubRe       = regexp.MustCompile(`(?:ghp_|gho_|ghu_|ghs_|ghr_)[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}`)
	awsRe          = regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}`)
	// Telegram bot tokens are 8-10 digits, a colon, then a token of
	// (currently) 35 characters. The tail is a length range with an
	// open upper end, not an exact length, so future token shapes
	// are not rejected.
	telegramRe = regexp.MustCompile(`\b\d{8,10}:[A-Za-z0-9_-]{30,}\b`)
	bearerRe   = regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._~-]{8,}`)
	basicRe    = regexp.MustCompile(`(?i)Basic\s+[A-Za-z0-9+/]{8,}={0,2}`)
	// Bare JWTs: header is always base64url of {"alg":...}, so it starts
	// with "eyJ"; each segment must be long enough to avoid matching
	// ordinary dotted words.
	jwtRe = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)
	// Credentials embedded in a URL: group 1 is "scheme://user:" (kept),
	// group 2 is the password (redacted).
	urlCredsRe = regexp.MustCompile(`(?i)(https?://[^\s/@:]+:)([^\s/@]+)@`)

	// Generic credential assignments. Group 1 is the key name, group 2 is
	// the separator (kept verbatim), group 3 is the secret value.
	// The (?i) flag makes key names case-insensitive. "auth" is
	// deliberately absent: "auth: <word>" is common prose and would be
	// redacted by mistake. The \b word boundaries keep the key from
	// matching inside a longer word ("token" in "mytoken" must not
	// match). Values of 4+ characters are redacted: short passwords
	// are still secrets, and the key/separator context means ordinary
	// prose ("password is hard to remember") never matches, because
	// it has no ":" or "=" separator.
	assignmentRe = regexp.MustCompile(`(?i)\b(api_key|apikey|access_key|token|password|passwd|secret)\b(\s*[:=]\s*['"]?)([^'"\s]{4,})`)

	// Generic high-entropy tokens: a long run of token characters.
	// Matching is deliberately conservative and the guard in
	// looksLikeHighEntropyToken does the real work: the run must mix
	// upper- and lowercase letters and digits, so hex digests (git
	// SHAs), UUIDs and ordinary long words are never matched. The
	// guard also skips runs that are a URL path segment (preceded by
	// "/"), so long document IDs in links survive. "/" is excluded
	// from the class so a URL can never merge host and path into one
	// long run; a standard-base64 token containing "/" still matches
	// on either side of the slash.
	genericTokenRe = regexp.MustCompile(`\b[A-Za-z0-9+=_-]{32,}\b`)
)

// secretPattern pairs a finding type with its detector.
type secretPattern struct {
	typ string
	re  *regexp.Regexp
}

var secretPatterns = []secretPattern{
	{"private_key", privateKeyRe},
	{"anthropic", anthropicRe},
	{"stripe", stripeRe},
	{"openai", openAIRe},
	{"github", githubRe},
	{"aws", awsRe},
	{"telegram", telegramRe},
	{"slack", slackRe},
	{"bearer", bearerRe},
	{"basic", basicRe},
	{"jwt", jwtRe},
	{"url_password", urlCredsRe},
	{"slack", slackWebhookRe},
	{"generic_token", genericTokenRe},
}

// looksLikeToken reports whether s looks like a real credential token
// rather than an ordinary English word. Bearer/Basic schemes are common
// prose ("Bearer authentication", "Basic training"), so the token after
// the scheme must be long enough and contain a digit, a base64 /
// punctuation character, or at least two uppercase letters. Random
// tokens essentially always satisfy this; ordinary words do not.
func looksLikeToken(s string) bool {
	if len(s) < 12 {
		return false
	}
	upper := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			return true
		case r == '+' || r == '/' || r == '=' || r == '.' || r == '_' || r == '-' || r == '~':
			return true
		case r >= 'A' && r <= 'Z':
			upper++
		}
	}
	return upper >= 2
}

// looksLikeHighEntropyToken reports whether s looks like a bare
// high-entropy credential rather than an ordinary long word, hash or
// identifier. It must be long and mix upper- and lowercase letters
// with digits: random tokens essentially always satisfy this, while
// hex digests (no uppercase), UUIDs (no uppercase) and long English
// words (no digits) do not.
func looksLikeHighEntropyToken(s string) bool {
	if len(s) < 32 {
		return false
	}
	var hasUpper, hasLower, hasDigit bool
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= 'a' && r <= 'z':
			hasLower = true
		case r >= '0' && r <= '9':
			hasDigit = true
		}
	}
	return hasUpper && hasLower && hasDigit
}

// redactGenericTokens redacts bare high-entropy tokens in s. Runs
// that are a URL path segment (preceded by "/") are left alone, so
// long document IDs in links are not redacted.
func redactGenericTokens(s string) string {
	locs := genericTokenRe.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for _, loc := range locs {
		m := s[loc[0]:loc[1]]
		if !looksLikeHighEntropyToken(m) || (loc[0] > 0 && s[loc[0]-1] == '/') {
			continue
		}
		b.WriteString(s[last:loc[0]])
		b.WriteString(Redacted)
		last = loc[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// redactSchemeToken redacts a "Scheme <token>" match: the scheme is
// kept, the token is replaced only when it looks like a real token;
// otherwise the match is returned unchanged (ordinary prose).
func redactSchemeToken(scheme, m string) string {
	if idx := strings.IndexFunc(m, unicode.IsSpace); idx >= 0 {
		if looksLikeToken(strings.TrimSpace(m[idx+1:])) {
			return scheme + " " + Redacted
		}
	}
	return m
}

// "[REDACTED]". Generic assignments keep the key name and separator and
// redact only the value (e.g. `api_key = hunter2secret` becomes
// `api_key = [REDACTED]`). Bearer/Basic keep the auth scheme and redact
// only the token; URL credentials keep the user and redact only the
// password. Ordinary words and URLs without credentials are left
// unchanged.
// redactMarkers: Go's regexp engine is slow on large inputs — a
// single ReplaceAllString over 1 MiB costs 50–400 ms per pattern, so
// the ~15 detectors in RedactSecrets cost ~1.5 s per MiB (and ~30 s
// under -race), paid on EVERY tool result the loop handles. Each
// detector below therefore has a literal marker that MUST be present
// for the detector to match at all; the regex only runs when its
// marker is present. A marker is a necessary condition, never a
// sufficient one: a present marker just runs the regex as before, so
// redaction behaviour is unchanged — an absent marker proves no
// match exists, so skipping the regex is safe. The markers are
// checked against the original text (replacements only ever remove
// text, and "[REDACTED]" introduces no marker), and the generic
// detectors use cheap byte scans instead of regexes. (Audit 3,
// Workstream 1: a 3 MiB file_read was eating the agent step budget
// in redaction alone.)
func hasLongTokenRun(s string) bool {
	// A generic-token match needs a run of 32+ token characters.
	run := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '=' || c == '_' || c == '-' {
			run++
			if run >= 32 {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}

func hasTelegramTokenShape(s string) bool {
	// A Telegram match needs a maximal digit run of 8–10 digits
	// immediately followed by ':'.
	run := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			run++
			continue
		}
		if c == ':' && run >= 8 && run <= 10 {
			return true
		}
		run = 0
	}
	return false
}

func RedactSecrets(text string) string {
	if text == "" {
		return text
	}
	// One lowercased copy serves every case-insensitive marker
	// check; strings.ToLower over 1 MiB costs ~2 ms, against
	// ~1.5 s for the unguarded regexes.
	lower := strings.ToLower(text)
	redacted := text
	// Private-key blocks first so their contents are never re-scanned.
	if strings.Contains(text, "-----BEGIN ") {
		redacted = privateKeyRe.ReplaceAllString(redacted, Redacted)
	}
	// Anthropic before OpenAI: sk-ant-... must not be swallowed by sk-.
	if strings.Contains(text, "sk-ant-") {
		redacted = anthropicRe.ReplaceAllString(redacted, Redacted)
	}
	if strings.Contains(lower, "sk_") || strings.Contains(lower, "rk_") {
		redacted = stripeRe.ReplaceAllString(redacted, Redacted)
	}
	if strings.Contains(text, "sk-") {
		redacted = openAIRe.ReplaceAllString(redacted, Redacted)
	}
	if strings.Contains(text, "ghp_") || strings.Contains(text, "gho_") ||
		strings.Contains(text, "ghu_") || strings.Contains(text, "ghs_") ||
		strings.Contains(text, "ghr_") || strings.Contains(text, "github_pat_") {
		redacted = githubRe.ReplaceAllString(redacted, Redacted)
	}
	if strings.Contains(text, "AKIA") || strings.Contains(text, "ASIA") {
		redacted = awsRe.ReplaceAllString(redacted, Redacted)
	}
	if hasTelegramTokenShape(text) {
		redacted = telegramRe.ReplaceAllString(redacted, Redacted)
	}
	if strings.Contains(text, "xox") {
		redacted = slackRe.ReplaceAllString(redacted, Redacted)
	}
	// Bearer/Basic tokens: keep the scheme, redact only the token — and
	// only when the token looks like a real token, so ordinary prose
	// such as "Bearer authentication" is left alone.
	if strings.Contains(lower, "bearer") {
		redacted = bearerRe.ReplaceAllStringFunc(redacted, func(m string) string {
			return redactSchemeToken("Bearer", m)
		})
	}
	if strings.Contains(lower, "basic") {
		redacted = basicRe.ReplaceAllStringFunc(redacted, func(m string) string {
			return redactSchemeToken("Basic", m)
		})
	}
	if strings.Contains(text, "eyJ") {
		redacted = jwtRe.ReplaceAllString(redacted, Redacted)
	}
	// URL credentials: keep "scheme://user:", redact only the password.
	if strings.Contains(text, "://") && strings.Contains(text, "@") {
		redacted = urlCredsRe.ReplaceAllString(redacted, "${1}"+Redacted+"@")
	}
	// Slack webhook URLs: keep the service prefix, redact the secret path.
	if strings.Contains(text, "hooks.slack.com") {
		redacted = slackWebhookRe.ReplaceAllString(redacted, "${1}"+Redacted)
	}
	// Bare high-entropy tokens, guarded by looksLikeHighEntropyToken.
	if hasLongTokenRun(text) {
		redacted = redactGenericTokens(redacted)
	}
	// Generic assignments: keep key name and separator, redact the value.
	// Values already replaced above read as "[REDACTED]" and stay redacted.
	if strings.Contains(lower, "api_key") || strings.Contains(lower, "apikey") ||
		strings.Contains(lower, "access_key") || strings.Contains(lower, "token") ||
		strings.Contains(lower, "password") || strings.Contains(lower, "passwd") ||
		strings.Contains(lower, "secret") {
		redacted = assignmentRe.ReplaceAllString(redacted, "${1}${2}"+Redacted)
	}
	return redacted
}

// ScanSecrets returns one Finding per distinct secret value found in
// text, in detector order (private_key, anthropic, stripe, openai,
// github, aws, telegram, slack, bearer, basic, jwt, url_password,
// generic_token, generic). A value already reported under a more
// specific type is not reported again as "generic".
func ScanSecrets(text string) []Finding {
	if text == "" {
		return nil
	}
	seen := make(map[string]bool)
	var findings []Finding
	add := func(typ, value string) {
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		findings = append(findings, Finding{Type: typ, Redacted: Redacted})
	}

	for _, p := range secretPatterns {
		switch p.typ {
		case "openai":
			// The OpenAI pattern also matches the "sk-" prefix of
			// Anthropic keys; those were already reported as anthropic.
			for _, m := range p.re.FindAllString(text, -1) {
				if strings.HasPrefix(m, "sk-ant-") {
					continue
				}
				add(p.typ, m)
			}
		case "bearer", "basic":
			// Record the token, not the auth scheme — and only when it
			// looks like a real token (see looksLikeToken).
			for _, m := range p.re.FindAllString(text, -1) {
				token := m
				if idx := strings.IndexFunc(m, unicode.IsSpace); idx >= 0 {
					token = strings.TrimSpace(m[idx+1:])
				}
				if looksLikeToken(token) {
					add(p.typ, token)
				}
			}
		case "url_password":
			// Record the password (group 2), not the scheme/user.
			for _, m := range p.re.FindAllStringSubmatch(text, -1) {
				if len(m) < 3 {
					continue
				}
				add(p.typ, m[2])
			}
		case "generic_token":
			// Bare high-entropy tokens, guarded by
			// looksLikeHighEntropyToken — and never double-report a
			// value (or part of one) already found under a more
			// specific type, e.g. the tail of a Telegram token.
			for _, loc := range p.re.FindAllStringIndex(text, -1) {
				m := text[loc[0]:loc[1]]
				if !looksLikeHighEntropyToken(m) || (loc[0] > 0 && text[loc[0]-1] == '/') {
					continue
				}
				dup := false
				for seenVal := range seen {
					if strings.Contains(m, seenVal) || strings.Contains(seenVal, m) {
						dup = true
						break
					}
				}
				if !dup {
					add(p.typ, m)
				}
			}
		default:
			for _, m := range p.re.FindAllString(text, -1) {
				add(p.typ, m)
			}
		}
	}

	// Generic assignments last: values already seen under a specific
	// type (e.g. api_key = sk-...) are not double-reported.
	for _, m := range assignmentRe.FindAllStringSubmatch(text, -1) {
		if len(m) < 4 {
			continue
		}
		add("generic", m[3])
	}
	return findings
}

// HasSecrets reports whether text contains any detectable secret.
func HasSecrets(text string) bool {
	return len(ScanSecrets(text)) > 0
}

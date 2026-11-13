// Package messaging — display redaction for the Telegram terminal card.
//
// Orion must never echo secrets into a chat, even when the model itself
// produced them: the Hermes screenshots show a Shell block echoing
// $(cat ~/.hermes/tokens/n8n-api.key). RedactDisplayText is applied to
// every piece of model/tool-derived text before it is rendered into a
// card, on top of security.RedactSecrets (which catches API-key-shaped
// values). It additionally redacts credential-file paths and
// credential assignments that RedactSecrets cannot know are secrets.
package messaging

import (
	"regexp"
	"strings"

	"github.com/irfan/orion/internal/security"
)

// credentialExtensions are file extensions that mark a path as a
// credential file, whatever directory it lives in.
var credentialExtensions = []string{".pem", ".key", ".p12", ".pfx"}

// credentialDirSegments are directory names that mark any path under
// them as credential-bearing.
var credentialDirSegments = map[string]bool{
	"tokens": true,
}

// credentialDotDirs are dot-directories under $HOME whose contents are
// credentials (private keys, cloud credentials, GPG keys).
var credentialDotDirs = []string{".ssh", ".gnupg", ".aws"}

// isCredentialPath reports whether p looks like a path to credential
// material: under ~/.ssh / ~/.gnupg / ~/.aws, under any directory named
// "tokens", or a file with a credential extension. Ordinary work paths
// (~/bin/pulse.sh, /home/irfan/Desktop/orion) return false.
func isCredentialPath(p string) bool {
	p = strings.Trim(p, `"'`+"`")
	p = strings.TrimRight(p, ",;:!?)")
	if p == "" {
		return false
	}
	for _, ext := range credentialExtensions {
		if strings.HasSuffix(p, ext) {
			return true
		}
	}
	// Normalise a leading $HOME so the segment checks below apply.
	norm := p
	norm = strings.TrimPrefix(norm, "$HOME")
	norm = strings.TrimPrefix(norm, "${HOME}")
	for _, seg := range strings.Split(norm, "/") {
		if credentialDirSegments[seg] {
			return true
		}
	}
	for _, d := range credentialDotDirs {
		if norm == "~"+"/"+d || strings.HasPrefix(norm, "~/"+d+"/") ||
			norm == "/"+d || strings.HasPrefix(norm, "/"+d+"/") ||
			strings.Contains(norm, "/"+d+"/") || strings.HasSuffix(norm, "/"+d) {
			return true
		}
	}
	return false
}

// pathRe matches path-like tokens: ~/..., /abs/..., and $HOME/....
// The match may carry trailing punctuation, which callers trim before
// deciding whether the path is a credential path.
var pathRe = regexp.MustCompile(`(?:~|\$HOME|\$\{HOME\})(?:/[A-Za-z0-9_.\-]+)+|/(?:[A-Za-z0-9_.\-]+/)*[A-Za-z0-9_.\-]+`)

// redactCredentialPaths replaces every credential-file path in s with
// "[redacted]", leaving ordinary paths untouched. A trailing
// punctuation character (comma, closing paren, ...) is preserved.
func redactCredentialPaths(s string) string {
	return pathRe.ReplaceAllStringFunc(s, func(m string) string {
		trimmed := strings.TrimRight(m, ",;:!?)]}")
		trail := m[len(trimmed):]
		if trimmed == "" {
			return m
		}
		if isCredentialPath(trimmed) {
			return "[redacted]" + trail
		}
		return m
	})
}

// catSubstRe matches $(cat <path>) command substitutions.
var catSubstRe = regexp.MustCompile(`\$\(\s*cat\s+([^)]+)\)`)

// backtickCatRe matches `cat <path>` backtick substitutions.
var backtickCatRe = regexp.MustCompile("`\\s*cat\\s+([^`]+)`")

// redactCatSubstitutions replaces $(cat <cred-path>) with
// "$(cat [redacted])" (and the backtick form likewise). A cat of an
// ordinary path is left alone; the general path rule below would only
// redact it if it were a credential path anyway.
func redactCatSubstitutions(s string) string {
	s = catSubstRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := catSubstRe.FindStringSubmatch(m)
		if len(sub) != 2 {
			return m
		}
		inner := strings.TrimSpace(sub[1])
		inner = strings.Trim(inner, `"'`)
		if isCredentialPath(inner) {
			return "$(cat [redacted])"
		}
		return m
	})
	s = backtickCatRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := backtickCatRe.FindStringSubmatch(m)
		if len(sub) != 2 {
			return m
		}
		inner := strings.TrimSpace(sub[1])
		inner = strings.Trim(inner, `"'`)
		if isCredentialPath(inner) {
			return "`cat [redacted]`"
		}
		return m
	})
	return s
}

// authHeaderRe matches "Authorization: <value>" (any scheme).
var authHeaderRe = regexp.MustCompile(`(?i)(Authorization:\s*)([^\s]+)`)

// credentialAssignRe matches api_key= / token= / secret= / password=
// assignments whose value survived RedactSecrets (e.g. a short or
// oddly-shaped value).
var credentialAssignRe = regexp.MustCompile(`(?i)((?:api[_-]?key|access[_-]?token|auth[_-]?token|token|secret|password)\s*=\s*)([^\s&"'` + "`" + `]+)`)

// RedactDisplayText redacts secrets and credential paths from text
// about to be shown in a Telegram card. It applies
// security.RedactSecrets first, then credential-path redaction
// (including $(cat ...) of such paths), Authorization header values,
// and api_key=/token= assignments. Ordinary paths and prose pass
// through unchanged.
func RedactDisplayText(s string) string {
	s = security.RedactSecrets(s)
	s = redactCatSubstitutions(s)
	s = redactCredentialPaths(s)
	s = authHeaderRe.ReplaceAllString(s, "${1}[redacted]")
	s = credentialAssignRe.ReplaceAllString(s, "${1}[redacted]")
	return s
}

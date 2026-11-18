package ci

import "regexp"

const redacted = "[REDACTED]"

var (
	reBearer  = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{8,}`)
	reURLCred = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s/@:]+:[^\s/@]+@`)
	rePrivKey = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|\z)`)
	reTokens  = regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b|gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}`)
	reKeyVal  = regexp.MustCompile(`(?i)\b([A-Za-z0-9_.-]*(?:password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key)[A-Za-z0-9_.-]*)(\s*[=:]\s*)("[^"\n]*"|'[^'\n]*'|[^\s"',;]+)`)
)

// Redact removes secrets from CI text: bearer tokens, credentials in URLs,
// private key blocks, known token and key shapes, and the value after a
// password/secret/token style key (the key name stays). It runs on pergent.sh's
// side, on top of Registry.Redact, and before any cut, so a cut can't split a
// token past it.
func Redact(s string) string {
	s = rePrivKey.ReplaceAllString(s, redacted)
	s = reBearer.ReplaceAllString(s, redacted)
	s = reURLCred.ReplaceAllString(s, "${1}"+redacted+"@")
	s = reTokens.ReplaceAllString(s, redacted)
	s = reKeyVal.ReplaceAllString(s, "${1}${2}"+redacted)
	return s
}

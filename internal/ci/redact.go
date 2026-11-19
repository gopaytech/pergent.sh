package ci

import "regexp"

const redacted = "[REDACTED]"

var (
	reBearer  = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{8,}`)
	reURLCred = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s/@:]+:[^\s/]+@`)
	rePrivKey = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|\z)`)
	// reKeyTail is a key body at the very start whose BEGIN line was left
	// off the front (Tail keeps the last 2 MiB): base64 lines, then END.
	reKeyTail = regexp.MustCompile(`\A(?:[A-Za-z0-9+/=]*[ \t]*\n)*-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	reBasic   = regexp.MustCompile(`(?i)(\bAuthorization["']?\s*[:=]\s*["']?Basic\s+)[A-Za-z0-9+/=]+`)
	reTokens  = regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b|gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}`)
	// A double-quoted value may hold escaped quotes (JSON): `"a\"b"` is one.
	reKeyDQ   = regexp.MustCompile(`(?i)\b([A-Za-z0-9_.-]*(?:password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key)[A-Za-z0-9_.-]*["']?\s*[=:]\s*)"(?:[^"\\\n]|\\.)*"`)
	reKeySQ   = regexp.MustCompile(`(?i)\b([A-Za-z0-9_.-]*(?:password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key)[A-Za-z0-9_.-]*["']?\s*[=:]\s*)'[^'\n]*'`)
	reKeyBare = regexp.MustCompile(`(?i)\b([A-Za-z0-9_.-]*(?:password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key)[A-Za-z0-9_.-]*["']?\s*[=:]\s*)[^\s"',;]+`)
	// A flag's value is a quoted string, whole, or one word.
	reFlag = regexp.MustCompile(`(?i)(--[A-Za-z0-9_-]*(?:password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key)[A-Za-z0-9_-]*\s+)(?:"(?:[^"\\\n]|\\.)*"|'[^'\n]*'|[^\s-]\S*)`)
)

// Redact removes secrets from CI text: bearer tokens and basic credentials,
// credentials in URLs, private key blocks (and a key body at the start whose
// BEGIN line was cut off), known token and key shapes, and the value after a
// password/secret/token style key (the key name stays). It runs on pergent.sh's
// side, on top of Registry.Redact, and before any cut, so a cut can't split a
// token past it.
func Redact(s string) string {
	s = rePrivKey.ReplaceAllString(s, redacted)
	s = reKeyTail.ReplaceAllString(s, redacted)
	s = reBearer.ReplaceAllString(s, redacted)
	s = reBasic.ReplaceAllString(s, "${1}"+redacted)
	s = reURLCred.ReplaceAllString(s, "${1}"+redacted+"@")
	s = reTokens.ReplaceAllString(s, redacted)
	s = reKeyDQ.ReplaceAllString(s, `${1}"`+redacted+`"`)
	s = reKeySQ.ReplaceAllString(s, `${1}'`+redacted+`'`)
	s = reKeyBare.ReplaceAllString(s, "${1}"+redacted)
	s = reFlag.ReplaceAllString(s, "${1}"+redacted)
	return s
}

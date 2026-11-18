package ci

import (
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	tailBuffer = 2 << 20
	tailLines  = 200
	textCap    = 16 << 10
)

var (
	reCSI     = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	reOSC     = regexp.MustCompile(`\x1b\][^\x07]*(?:\x07|\x1b\\)`)
	reSection = regexp.MustCompile(`section_(?:start|end):\d+:[^\r\n]*\r?\n?`)
)

// clean makes CI text safe to attach: valid UTF-8, no ANSI or OSC sequences, no
// GitLab section markers, CRLF and lone CR as LF, no control byte but \t and \n
// (opencode's read tool treats a file with NUL or many controls as binary), and
// redacted. Redaction comes before any cut.
func clean(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = reOSC.ReplaceAllString(s, "")
	s = reCSI.ReplaceAllString(s, "")
	s = reSection.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.Map(func(r rune) rune {
		if (r < 0x20 && r != '\t' && r != '\n') || r == 0x7f {
			return -1
		}
		return r
	}, s)
	return Redact(s)
}

// Tail reads a log through a rolling 2 MiB buffer, so the text is the real end
// of the log, and keeps its last 200 lines and at most 16 KiB. cut says
// something was left off the front.
func Tail(r io.Reader) (string, bool, error) {
	var buf []byte
	cut := false
	chunk := make([]byte, 64<<10)
	for {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if len(buf) > 2*tailBuffer {
			buf = append(buf[:0], buf[len(buf)-tailBuffer:]...)
			cut = true
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", false, err
		}
	}
	if len(buf) > tailBuffer {
		buf = buf[len(buf)-tailBuffer:]
		cut = true
	}
	s := string(buf)
	if cut {
		s = s[runeStart(s, 0):]
	}
	s = strings.TrimRight(clean(s), "\n")
	if lines := strings.Split(s, "\n"); len(lines) > tailLines {
		s = strings.Join(lines[len(lines)-tailLines:], "\n")
		cut = true
	}
	if len(s) > textCap {
		s = s[runeStart(s, len(s)-textCap):]
		cut = true
	}
	return s, cut, nil
}

// Cap cleans a report and keeps its first 16 KiB, cut on a rune start.
func Cap(s string) (string, bool) {
	s = clean(s)
	if len(s) <= textCap {
		return s, false
	}
	end := textCap
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end], true
}

// runeStart is the first index at or after i that starts a rune.
func runeStart(s string, i int) int {
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}

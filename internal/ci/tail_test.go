package ci

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTail(t *testing.T) {
	t.Run("last 200 lines", func(t *testing.T) {
		var b strings.Builder
		for i := 1; i <= 300; i++ {
			fmt.Fprintf(&b, "line %d\n", i)
		}
		got, cut, err := Tail(strings.NewReader(b.String()))
		if err != nil || !cut {
			t.Fatalf("err=%v cut=%v", err, cut)
		}
		lines := strings.Split(got, "\n")
		if len(lines) != 200 || lines[0] != "line 101" || lines[199] != "line 300" {
			t.Fatalf("got %d lines, first %q last %q", len(lines), lines[0], lines[len(lines)-1])
		}
	})
	t.Run("short is not cut", func(t *testing.T) {
		got, cut, _ := Tail(strings.NewReader("a\nb\n"))
		if cut || got != "a\nb" {
			t.Fatalf("got %q cut=%v", got, cut)
		}
	})
	t.Run("3 MiB keeps real last line", func(t *testing.T) {
		body := strings.Repeat("x", 3<<20) + "\nthe real end"
		got, cut, err := Tail(strings.NewReader(body))
		if err != nil || !cut || !strings.HasSuffix(got, "the real end") {
			t.Fatalf("err=%v cut=%v tail=%q", err, cut, got[max(0, len(got)-20):])
		}
	})
	t.Run("ansi", func(t *testing.T) {
		got, _, _ := Tail(strings.NewReader("\x1b[31mred\x1b[0m \x1b]0;title\x07ok"))
		if got != "red ok" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("section markers", func(t *testing.T) {
		in := "section_start:1700000000:build\r\x1b[0K\nreal\nsection_end:1700000001:build\r\x1b[0K\n"
		got, _, _ := Tail(strings.NewReader(in))
		if strings.Contains(got, "section_") || got != "real" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("crlf", func(t *testing.T) {
		got, _, _ := Tail(strings.NewReader("a\r\nb\r\n"))
		if got != "a\nb" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("invalid utf8", func(t *testing.T) {
		got, _, _ := Tail(strings.NewReader("a\xffb"))
		if !utf8.ValidString(got) {
			t.Fatalf("invalid: %q", got)
		}
	})
	t.Run("single long line", func(t *testing.T) {
		got, cut, _ := Tail(strings.NewReader("a" + strings.Repeat("é", 10000)))
		if !cut || len(got) > 16<<10 || !utf8.ValidString(got) || strings.ContainsRune(got, utf8.RuneError) {
			t.Fatalf("len=%d cut=%v", len(got), cut)
		}
	})
	t.Run("control bytes", func(t *testing.T) {
		got, _, _ := Tail(strings.NewReader("a\x00b\x01c\x7fd\x08e\rf"))
		if err := noControl(got); err != "" {
			t.Fatal(err)
		}
	})
	t.Run("token across the cut is redacted", func(t *testing.T) {
		tok := "ghp_" + strings.Repeat("Ab1", 12)
		body := strings.Repeat("a", 16<<10-7) + tok
		got, _, _ := Tail(strings.NewReader(body))
		if strings.Contains(got, "ghp_Ab1") || strings.Contains(got, "Ab1Ab1") {
			t.Fatalf("token leaked: %q", got[len(got)-20:])
		}
	})
}

func TestCap(t *testing.T) {
	got, cut := Cap("hello\x00 \x1b[1mworld\x1b[0m")
	if cut || got != "hello world" {
		t.Fatalf("got %q cut=%v", got, cut)
	}
	got, cut = Cap(strings.Repeat("é", 20000))
	if !cut || len(got) > 16<<10 || !utf8.ValidString(got) || strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("len=%d cut=%v", len(got), cut)
	}
	tok := "ghp_" + strings.Repeat("Ab1", 12)
	got, _ = Cap(strings.Repeat("a", 16<<10-7) + tok)
	if strings.Contains(got, "ghp_") {
		t.Fatalf("token leaked across the cut")
	}
}

// noControl returns a message when s holds a C0 control byte other than \t
// and \n, or DEL.
func noControl(s string) string {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 0x20 && c != '\t' && c != '\n') || c == 0x7f {
			return fmt.Sprintf("control byte %#x at %d", c, i)
		}
	}
	return ""
}

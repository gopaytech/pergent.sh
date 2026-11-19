package ci

import (
	"strings"
	"testing"
)

func TestBuild(t *testing.T) {
	t.Run("failed log job has its tail in a fence", func(t *testing.T) {
		out := string(Build(Snapshot{Head: "abc123", Jobs: []Job{{Name: "test", Read: "log", Result: Failed, Blocks: true, Log: "boom\nbad"}}}))
		for _, w := range []string{"# CI job results\n", "Head commit: abc123", "## test: failed (blocks approval)", "Log tail:", "```text\nboom\nbad\n```"} {
			if !strings.Contains(out, w) {
				t.Errorf("missing %q in:\n%s", w, out)
			}
		}
	})
	t.Run("passed log job has a heading only", func(t *testing.T) {
		out := string(Build(Snapshot{Head: "h", Jobs: []Job{{Name: "test", Read: "log", Result: Passed, Blocks: true, Log: "noise"}}}))
		if !strings.Contains(out, "## test: passed\n") || strings.Contains(out, "noise") || strings.Contains(out, "```") || strings.Contains(out, "blocks approval") {
			t.Fatalf("got:\n%s", out)
		}
	})
	t.Run("report job has its report", func(t *testing.T) {
		out := string(Build(Snapshot{Head: "h", Jobs: []Job{{Name: "scan", Read: "report", Result: Passed, Report: "{\"a\":1}", Cut: true}}}))
		if !strings.Contains(out, "Report:") || !strings.Contains(out, "{\"a\":1}") || !strings.Contains(out, "_Cut to the first 16 KiB._") {
			t.Fatalf("got:\n%s", out)
		}
	})
	t.Run("log cut note", func(t *testing.T) {
		out := string(Build(Snapshot{Head: "h", Jobs: []Job{{Name: "t", Read: "log", Result: Failed, Log: "x", Cut: true}}}))
		if !strings.Contains(out, "_Cut to the last 16 KiB._") {
			t.Fatalf("got:\n%s", out)
		}
	})
	t.Run("longer fence", func(t *testing.T) {
		out := string(Build(Snapshot{Head: "h", Jobs: []Job{{Name: "t", Read: "log", Result: Failed, Log: "a\n```\nb"}}}))
		if !strings.Contains(out, "````text\na\n```\nb\n````\n") {
			t.Fatalf("got:\n%s", out)
		}
	})
	t.Run("64 KiB limit", func(t *testing.T) {
		big := strings.Repeat("y", 16<<10-1)
		var jobs []Job
		for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
			jobs = append(jobs, Job{Name: n, Read: "log", Result: Failed, Log: big})
		}
		out := string(Build(Snapshot{Head: "h", Jobs: jobs}))
		if len(out) > 64<<10 {
			t.Fatalf("size %d", len(out))
		}
		for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
			if !strings.Contains(out, "## "+n+": failed") {
				t.Errorf("heading %s missing", n)
			}
		}
		if !strings.Contains(out, "_Text left out: the file's 64 KiB limit._") {
			t.Error("note missing")
		}
		if !strings.Contains(out, "## a: failed\n\nLog tail:") {
			t.Error("first text must stay")
		}
		if strings.Count(out, "_Text left out") < 1 || strings.Contains(out[strings.Index(out, "## f:"):], "Log tail:") {
			t.Error("last text must be left out")
		}
	})
	t.Run("reason in heading", func(t *testing.T) {
		out := string(Build(Snapshot{Head: "h", Jobs: []Job{{Name: "api", Read: "log", Result: Unreadable, Reason: "no permission", Blocks: true}}}))
		if !strings.Contains(out, "## api: unreadable (no permission) (blocks approval)") {
			t.Fatalf("got:\n%s", out)
		}
	})
	t.Run("status read has no text", func(t *testing.T) {
		out := string(Build(Snapshot{Head: "h", Jobs: []Job{{Name: "s", Read: "status", Result: Failed, Log: "SECRETTEXT", Report: "MORE"}}}))
		if strings.Contains(out, "SECRETTEXT") || strings.Contains(out, "MORE") {
			t.Fatalf("got:\n%s", out)
		}
	})
	t.Run("redacts", func(t *testing.T) {
		out := string(Build(Snapshot{Head: "h", Jobs: []Job{{Name: "t", Read: "log", Result: Failed, Log: "password=hunter2"}}}))
		if strings.Contains(out, "hunter2") {
			t.Fatal("not redacted")
		}
	})
	t.Run("no control bytes", func(t *testing.T) {
		dirty := "a\x00b\x1b[31mred\x1b[0m\x1b]0;t\x07c\x01d\x7fe\rf\x08"
		out := string(Build(Snapshot{Head: "h\x00", Jobs: []Job{
			{Name: "l\x00", Read: "log", Result: Failed, Log: dirty},
			{Name: "r", Read: "report", Result: Passed, Report: dirty},
		}}))
		if msg := noControl(out); msg != "" {
			t.Fatal(msg, "\n", out)
		}
		if strings.Contains(out, "[31m") {
			t.Fatal("ANSI left")
		}
	})
}

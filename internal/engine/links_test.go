package engine

import "testing"

func TestFileURL(t *testing.T) {
	cases := []struct {
		name      string
		github    bool
		changeURL string
		want      string
	}{
		{"github", true, "https://github.com/o/r/pull/11", "https://github.com/o/r/blob/abc123/internal/a.go?plain=1#L3-L5"},
		{"github enterprise", true, "https://ghe.example.com/o/r/pull/7", "https://ghe.example.com/o/r/blob/abc123/internal/a.go?plain=1#L3-L5"},
		{"gitlab", false, "https://gitlab.example.com/g/sub/p/-/merge_requests/3", "https://gitlab.example.com/g/sub/p/-/blob/abc123/internal/a.go?plain=1#L3-5"},
		{"unknown page", true, "", ""},
		{"gitlab page on github", true, "https://gitlab.example.com/g/p/-/merge_requests/3", ""},
		{"github page on gitlab", false, "https://github.com/o/r/pull/11", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			link := fileURL(c.github, c.changeURL, "abc123")
			got := ""
			if link != nil {
				got = link("internal/a.go", 3, 5)
			}
			if got != c.want {
				t.Errorf("fileURL(%v, %q) link = %q, want %q", c.github, c.changeURL, got, c.want)
			}
		})
	}
}

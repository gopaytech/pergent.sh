package dashboard

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
)

func TestWindowBody(t *testing.T) {
	t.Parallel()
	const bothMsg = "Set both Context window and Max output, or neither."
	tests := []struct {
		name, context, output string
		beforeC, beforeO      int
		wantC, wantO          int
		wantOK                bool
		wantMsg               string // exact (the context message, the pair message) or a part
	}{
		{name: "absent keeps both", beforeC: 1_000_000, beforeO: 64_000, wantC: 1_000_000, wantO: 64_000, wantOK: true},
		{name: "both absent, none before", wantOK: true},
		{name: "null both clears both", context: "null", output: "null", beforeC: 1_000_000, beforeO: 64_000, wantOK: true},
		{name: "null context with output kept is refused", context: "null", beforeC: 1_000_000, beforeO: 500_000, wantMsg: bothMsg},
		{name: "null output with context kept is refused", output: "null", beforeC: 1_000_000, beforeO: 500_000, wantMsg: bothMsg},
		{name: "lowest pair", context: "4096", output: "1024", wantC: 4096, wantO: 1024, wantOK: true},
		{name: "output as large as the context", context: "10000000", output: "10000000", wantC: 10_000_000, wantO: 10_000_000, wantOK: true},
		{name: "context alone is refused", context: "10000000", wantMsg: bothMsg},
		{name: "output alone is refused", output: "2000000", wantMsg: bothMsg},
		{name: "set both on an empty entry", context: "1000000", output: "64000", wantC: 1_000_000, wantO: 64_000, wantOK: true},
		{name: "context 4095", context: "4095", output: "1024", wantMsg: msgBadContextWindow},
		{name: "context 10000001", context: "10000001", output: "1024", wantMsg: msgBadContextWindow},
		{name: "context 0", context: "0", output: "1024", wantMsg: msgBadContextWindow},
		{name: "context -1", context: "-1", output: "1024", wantMsg: msgBadContextWindow},
		{name: "context 1e6", context: "1e6", output: "1024", wantMsg: msgBadContextWindow},
		{name: "context 1000000.0", context: "1000000.0", output: "1024", wantMsg: msgBadContextWindow},
		{name: "context as text", context: `"1000000"`, output: "1024", wantMsg: msgBadContextWindow},
		{name: "context too big for an int", context: "9999999999999999999", output: "1024", wantMsg: msgBadContextWindow},
		{name: "output 1023", context: "4096", output: "1023", wantMsg: "from 1024 to 4096"},
		{name: "output 1023, no context", output: "1023", wantMsg: "from 1024 to 10000000"},
		{name: "output above the entry's context", output: "1000001", beforeC: 1_000_000, beforeO: 2000, wantMsg: "to 1000000, the entry's context window"},
		{name: "context drops under the kept output", context: "100000", beforeC: 1_000_000, beforeO: 200_000, wantMsg: "maxOutput must be null or a whole number from 1024 to 100000"},
		{name: "output as text", context: "4096", output: `"2000"`, wantMsg: "maxOutput must be null"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			c, o, ok := windowBody(rec, []byte(tt.context), []byte(tt.output), tt.beforeC, tt.beforeO)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (body %s)", ok, tt.wantOK, rec.Body)
			}
			if ok {
				if c != tt.wantC || o != tt.wantO {
					t.Errorf("got (%d, %d), want (%d, %d)", c, o, tt.wantC, tt.wantO)
				}
				return
			}
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tt.wantMsg) {
				t.Errorf("body %s lacks %q", rec.Body, tt.wantMsg)
			}
			if strings.Contains(strings.ToLower(rec.Body.String()), "limit") {
				t.Errorf("body %s calls the fields limits", rec.Body)
			}
		})
	}
}

func TestWindowChanges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		bc, bo, c, o int
		want         []string
	}{
		{"set both", 0, 0, 1_000_000, 64_000, []string{"context_window none to 1000000", "max_output none to 64000"}},
		{"clear one", 1_000_000, 64_000, 1_000_000, 0, []string{"max_output 64000 to none"}},
		{"no change", 1_000_000, 64_000, 1_000_000, 64_000, nil},
	}
	for _, tt := range tests {
		if got := windowChanges(tt.bc, tt.bo, tt.c, tt.o); !slices.Equal(got, tt.want) {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestTokensOrNull(t *testing.T) {
	t.Parallel()
	if tokensOrNull(0) != nil {
		t.Error("0 must be null")
	}
	if p := tokensOrNull(4096); p == nil || *p != 4096 {
		t.Errorf("4096 = %v", p)
	}
}

// The gate is a package variable: this test is serial.
func TestModelWindowOff(t *testing.T) {
	old := config.ModelWindowMinVersion
	t.Cleanup(func() { config.ModelWindowMinVersion = old })

	config.ModelWindowMinVersion = ""
	const want = "Jobs get no context window or max output for now: no pergent image reads them yet."
	if got := modelWindowOff("r/pergent:v0.14.0", "r/pergent-security:v0.14.0"); got != want {
		t.Errorf("no floor = %q, want %q", got, want)
	}
	config.ModelWindowMinVersion = "v0.14.0"
	if got := modelWindowOff("r/pergent:v0.14.0", "r/pergent-security:v0.14.0"); got != "" {
		t.Errorf("both pass = %q", got)
	}
	if got := modelWindowOff("r/pergent:v0.13.0", "r/pergent-security:v0.14.0"); !strings.HasPrefix(got, "Reviews get no context window or max output for now: ") {
		t.Errorf("review refused = %q", got)
	}
	got := modelWindowOff("r/pergent:v0.14.0", "r/pergent-security@sha256:abc123")
	if !strings.HasPrefix(got, "Security scans get no context window or max output for now: ") || !strings.Contains(got, "no tag") {
		t.Errorf("scan refused = %q", got)
	}
}

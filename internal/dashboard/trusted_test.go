package dashboard

import (
	"testing"

	"github.com/gopaytech/pergent.sh/internal/store"
)

// A waiting run says what it waits for.
func TestJudgeWaiting(t *testing.T) {
	for reason, want := range map[string]string{
		store.WaitUntrusted:   "Waiting for someone with write access to start the review.",
		store.WaitCheckFailed: "Couldn't check the author's access. Someone with write access can start the review.",
	} {
		got := judge(store.RunDigest{Outcome: OutcomeWaiting, State: store.Waiting, SkipReason: reason})
		if got.outcome != OutcomeWaiting || got.reason != want || got.status != "" {
			t.Errorf("%s: judge = %+v, want waiting / %q", reason, got, want)
		}
	}
}

package dashboard

import (
	"testing"

	"github.com/gopaytech/pergent.sh/internal/store"
)

// A run that couldn't be posted shows the final status its give-up recorded:
// neutral when the repository asked for it, failed otherwise, and failed
// when the recorded status is anything else (an earlier decision of the run).
func TestJudgePublishFailedStatus(t *testing.T) {
	for recorded, want := range map[string]string{
		"neutral": "neutral",
		"failed":  statusFailed,
		"":        statusFailed,
		"success": statusFailed,
	} {
		got := judge(store.RunDigest{Outcome: OutcomeFailed, State: store.PublishFailed, DecisionStatus: recorded})
		if got.outcome != OutcomeFailed || got.reason != reasonNotPosted || got.status != want {
			t.Errorf("judge(recorded %q) = %+v; want failed / %q / %q", recorded, got, reasonNotPosted, want)
		}
	}
}

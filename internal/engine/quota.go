package engine

// What people read on a change when a monthly token quota is used up. None of
// it holds a number, a limit or the name of a repository or instance: on a
// public repository anyone reads it.
const (
	// quotaDescription is the pending status on the head of a held change.
	quotaDescription = "Monthly token quota used up. An admin can start the review in the dashboard."
	// quotaNote is the one note a change gets in a month, and the reply of a
	// command that was refused while the note wasn't posted yet.
	quotaNote = "Not reviewed: the monthly token quota is used up. Reviews start again on the 1st, or an admin can start this one from the pergent.sh dashboard."
	// whyQuota is the outcome of a command refused for the quota, as the
	// commands table says it ("refused: " + why).
	whyQuota = "the monthly token quota is used up"
)

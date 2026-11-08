package store

// SkillUsage is what one skill used in one run, as the Job reported it.
// CacheRead, CacheWrite and Reasoning are nil when the Job didn't report
// them.
type SkillUsage struct {
	Skill, Verdict                   string
	In, Out                          int64
	CacheRead, CacheWrite, Reasoning *int64
	Findings, Blocking               int
}

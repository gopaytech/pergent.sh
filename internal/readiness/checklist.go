package readiness

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/gopaytech/pergent.sh/internal/inputs"
)

// The state of a checklist item. Unknown is "couldn't check": it is never
// shown as passed and never counted as passed.
const (
	Passed  = "passed"
	Warning = "warning"
	Missing = "missing"
	Unknown = "unknown"
)

// PodItem is one settings check the pod read at scan time. Count is the
// approvals needed (approvals only).
type PodItem struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	Count  int    `json:"count,omitempty"`
}

// PodResult is what the pod read from the platform at scan time.
type PodResult struct {
	Version        int     `json:"version"`
	Approvals      PodItem `json:"approvals"`
	Protected      PodItem `json:"protected"`
	RequiredChecks PodItem `json:"requiredChecks"`
}

// Flags are the readiness items that go to Needs attention. BudgetKnown is
// false when there is no file result, or an effective skill has a Problem so
// its size is not known; Budget is then only what is known.
type Flags struct {
	NoCoverage, NoContext, BudgetKnown bool
	Budget                             int
}

// ChecklistInput is everything the checklist is built from.
type ChecklistInput struct {
	Pod            *PodResult
	Job            *JobResult // nil: no file result
	FileError      string     // the scan's error when Job is nil
	Library        []string   // the library list now: the repository's, or the defaults
	RepoSkills     []string
	LibrarySizes   map[string]int
	SingleApproval bool // the live single_approval item is set
}

// Item is one line of the checklist. Reason is the couldn't-check reason or
// what was found. Why and Fix are only set when the item is not passed.
type Item struct {
	Key    string      `json:"key"`
	State  string      `json:"state"`
	Reason string      `json:"reason"`
	Why    string      `json:"why"`
	Fix    string      `json:"fix"`
	List   []string    `json:"list,omitempty"`
	Noise  []NoiseFile `json:"noise,omitempty"`
	More   int         `json:"more,omitempty"`
}

// Section is a group of items: "context", "process", "skills" or "size".
type Section struct {
	Key   string `json:"key"`
	Items []Item `json:"items"`
}

// Checklist is the Readiness tab. Suggested names library skills that fit the
// files and are not turned on. Budget is the size of skills plus instructions
// (see Flags.BudgetKnown).
type Checklist struct {
	Sections  []Section `json:"sections"`
	Suggested []string  `json:"suggested"`
	Budget    int       `json:"budget"`
}

// analysis is what both ComputeFlags and BuildChecklist read from a scan.
type analysis struct {
	usable     bool       // there is a file result
	effective  []SkillFit // the skills a review would run
	covers     bool       // an effective skill covers the repository
	notScanned bool       // an effective skill is one the scan has not seen
	sizeWhy    string     // why the size is not known, when BudgetKnown is false
	flags      Flags
}

func analyse(job *JobResult, library, repoSkills []string, librarySizes map[string]int) analysis {
	if job == nil || job.Error != "" {
		return analysis{}
	}
	a := analysis{usable: true, effective: Effective(job.Skills, library, repoSkills)}
	a.flags.BudgetKnown = true
	a.flags.Budget = job.InstructionsBytes
	unknown := func(why string) {
		if a.flags.BudgetKnown {
			a.sizeWhy = why
		}
		a.flags.BudgetKnown = false
	}
	// librarySize adds a library skill's size; one the library no longer has
	// (deleted after it was turned on) has no size we know.
	librarySize := func(name string) {
		size, ok := librarySizes[name]
		if !ok {
			unknown(fmt.Sprintf("couldn't add up the size: the size of skill %q is not known", name))
		}
		a.flags.Budget += size
	}
	for _, f := range a.effective {
		a.covers = a.covers || f.Covers()
		switch {
		case f.Problem == ProblemNotScanned:
			a.notScanned = true
			librarySize(f.Name) // the library knows its size
		case f.Problem != "":
			unknown(fmt.Sprintf("couldn't add up the size: skill %q: %s", f.Name, f.Problem))
		case f.Folder == inputs.SourceLibrary:
			librarySize(f.Name)
		default:
			a.flags.Budget += f.Bytes
		}
	}
	c := job.Context
	a.flags.NoContext = (c.AgentFile == "" || c.AgentFileEmpty) && c.Readme == ""
	// No effective skill is the review's own item (no_skills). A skill the scan
	// has not seen may cover, so that is no gap either.
	a.flags.NoCoverage = len(a.effective) > 0 && !a.covers && !a.notScanned
	return a
}

// ComputeFlags raises the attention flags from the file result and the skills
// turned on now. Without a file result nothing is raised.
func ComputeFlags(job *JobResult, library, repoSkills []string, librarySizes map[string]int) Flags {
	return analyse(job, library, repoSkills, librarySizes).flags
}

// BuildChecklist turns a scan's results into the checklist.
func BuildChecklist(in ChecklistInput) Checklist {
	job, fileError := in.Job, in.FileError
	if job != nil && job.Error != "" {
		if fileError == "" {
			fileError = job.Error
		}
		job = nil
	}
	a := analyse(job, in.Library, in.RepoSkills, in.LibrarySizes)

	fileOK := func(key string) (Item, bool) {
		if job != nil {
			return Item{Key: key}, true
		}
		reason := fileError
		if reason == "" {
			reason = "the files were not scanned"
		}
		return Item{Key: key, State: Unknown, Reason: "couldn't scan files: " + reason}, false
	}

	var c Checklist
	c.Budget = a.flags.Budget
	if job != nil {
		c.Suggested = suggested(job, a.effective)
	}
	c.Sections = []Section{
		{Key: "context", Items: []Item{
			finish(agentFileItem(fileOK("agent_file"))(job)),
			finish(readmeItem(fileOK("readme"))(job)),
			finish(contributingItem(fileOK("contributing"))(job)),
			finish(instructionsItem(fileOK("instructions"))(job)),
		}},
		{Key: "process", Items: []Item{
			finish(codeOwnersItem(fileOK("codeowners"))(job)),
			finish(approvalsItem(in)),
			finish(podItem("protected", in.Pod, func(p *PodResult) PodItem { return p.Protected }, whyProtected, fixProtected)),
			finish(podItem("required_checks", in.Pod, func(p *PodResult) PodItem { return p.RequiredChecks }, whyChecks, fixChecks)),
		}},
		{Key: "skills", Items: []Item{
			finish(skillMatchItem(fileOK("skill_match"))(a)),
			finish(unmatchedItem(fileOK("skills_unmatched"))(a)),
			finish(languagesItem(fileOK("languages"))(job)),
		}},
		{Key: "size", Items: []Item{
			finish(noiseItem(fileOK("noise"))(job)),
			finish(budgetItem(fileOK("budget"))(a)),
		}},
	}
	return c
}

// The advice. Why is one sentence on what the gap costs a review.
const (
	whyAgentFile    = "Without an agent context file, a review has to guess what the project is and how it is built."
	fixAgentFile    = "Add AGENTS.md (or CLAUDE.md) at the root: what the project does, how to build and test it, and the rules a change should follow."
	whyReadme       = "The README is where a review first learns what the project is for."
	fixReadme       = "Add a README at the root that says what the project is and how to run it."
	whyContributing = "A contributing guide tells a review which conventions a change should follow."
	fixContributing = "Add CONTRIBUTING.md at the root, in .github/ or in docs/."
	whyInstructions = "Without review instructions, a review works from the skills alone and does not know what matters most in this repository."
	fixInstructions = "Add .pergent/instructions.md with the review focus: risky areas and the rules to enforce."
	whyCodeOwners   = "Without CODEOWNERS, nothing makes a person who owns the code approve a change, so the bot's approval can be the only one."
	fixCodeOwners   = "Add a CODEOWNERS file (root, .github/, docs/ or .gitlab/) with at least one rule."
	whyApprovals    = "When one approval or none is enough, the bot's approval alone can merge a change."
	fixApprovals    = "Require at least 2 approvals to merge in the repository's merge settings."
	whyProtected    = "An unprotected default branch accepts direct pushes, which skip the review."
	fixProtected    = "Protect the default branch against direct pushes."
	whyChecks       = "Without a required check, a change can merge before its checks pass."
	fixChecks       = "GitHub: require at least one status check in branch protection. GitLab: turn on \"Pipelines must succeed\"."
	whySkillMatch   = "When no skill matches the repository's files, a review has nothing to check the code against and comes back thin."
	fixSkillMatch   = "Turn on a library skill whose paths match this repository's files, or add a skill under .pergent/skills."
	whyUnmatched    = "A skill that matches no file adds to the size of every review and checks nothing."
	fixUnmatched    = "Turn it off for this repository, or fix its paths."
	whyLanguages    = "Code in a language no skill covers is reviewed without guidance."
	fixLanguages    = "Turn on a skill for each language, or add one to the library."
	whyNoise        = "Generated, vendored and very large files fill the review's input and bury the real changes."
	whyBudget       = "Over 900 KiB, the review Job cannot take its inputs and the review fails."
	fixBudget       = "Turn off skills or shorten .pergent/instructions.md until the total is 900 KiB or less."
)

// finish drops the advice of a passed item.
func finish(it Item) Item {
	if it.State == Passed {
		it.Why, it.Fix = "", ""
	}
	return it
}

func (it Item) with(state, reason, why, fix string) Item {
	it.State, it.Reason, it.Why, it.Fix = state, reason, why, fix
	return it
}

// Each file item is built in 2 steps: fileOK gives the item, or the unknown
// item when there is no file result, and the builder fills it.

func agentFileItem(it Item, ok bool) func(*JobResult) Item {
	return func(job *JobResult) Item {
		if !ok {
			return it
		}
		c := job.Context
		switch {
		case c.AgentFile == "":
			return it.with(Missing, "no AGENTS.md or CLAUDE.md at the root", whyAgentFile, fixAgentFile)
		case c.AgentFileEmpty:
			return it.with(Missing, c.AgentFile+" is empty", whyAgentFile, fixAgentFile)
		}
		return it.with(Passed, c.AgentFile, "", "")
	}
}

func readmeItem(it Item, ok bool) func(*JobResult) Item {
	return func(job *JobResult) Item {
		if !ok {
			return it
		}
		if job.Context.Readme == "" {
			return it.with(Missing, "no README at the root", whyReadme, fixReadme)
		}
		return it.with(Passed, job.Context.Readme, "", "")
	}
}

func contributingItem(it Item, ok bool) func(*JobResult) Item {
	return func(job *JobResult) Item {
		if !ok {
			return it
		}
		if job.Context.Contributing == "" {
			return it.with(Warning, "no CONTRIBUTING.md at the root, in .github/ or in docs/", whyContributing, fixContributing)
		}
		return it.with(Passed, job.Context.Contributing, "", "")
	}
}

func instructionsItem(it Item, ok bool) func(*JobResult) Item {
	return func(job *JobResult) Item {
		if !ok {
			return it
		}
		if !job.Context.Instructions {
			return it.with(Warning, "no .pergent/instructions.md", whyInstructions, fixInstructions)
		}
		return it.with(Passed, ".pergent/instructions.md", "", "")
	}
}

func codeOwnersItem(it Item, ok bool) func(*JobResult) Item {
	return func(job *JobResult) Item {
		if !ok {
			return it
		}
		c := job.Context
		switch {
		case c.CodeOwners == "":
			return it.with(Warning, "no CODEOWNERS file", whyCodeOwners, fixCodeOwners)
		case c.CodeOwnerRules == 0:
			return it.with(Warning, c.CodeOwners+" has no rules", whyCodeOwners, fixCodeOwners)
		}
		return it.with(Passed, fmt.Sprintf("%s, %d rules", c.CodeOwners, c.CodeOwnerRules), "", "")
	}
}

// podState keeps a state the pod can set; any other is Unknown, never passed.
func podState(s string) string {
	switch s {
	case Passed, Warning, Missing:
		return s
	}
	return Unknown
}

const (
	noSettings   = "couldn't read settings: the scan did not read them"
	cantCheckPod = "couldn't check this setting"
)

func podItem(key string, pod *PodResult, pick func(*PodResult) PodItem, why, fix string) Item {
	it := Item{Key: key}
	if pod == nil {
		return it.with(Unknown, noSettings, "", "")
	}
	p := pick(pod)
	state := podState(p.State)
	reason := p.Reason
	if state == Unknown {
		if reason == "" {
			reason = cantCheckPod
		}
		return it.with(Unknown, reason, "", "")
	}
	return it.with(state, reason, why, fix)
}

func approvalsItem(in ChecklistInput) Item {
	it := Item{Key: "approvals"}
	if in.SingleApproval {
		// The hourly check is newer than the scan and wins.
		return it.with(Warning, "one approval is enough to merge (found by the hourly check)", whyApprovals, fixApprovals)
	}
	if in.Pod == nil {
		return it.with(Unknown, noSettings, "", "")
	}
	p := in.Pod.Approvals
	if podState(p.State) == Unknown {
		reason := p.Reason
		if reason == "" {
			reason = cantCheckPod
		}
		return it.with(Unknown, reason, "", "")
	}
	// The count decides, whatever state the pod set.
	reason := p.Reason
	switch {
	case p.Count >= 2:
		if reason == "" {
			reason = fmt.Sprintf("%d approvals needed to merge", p.Count)
		}
		return it.with(Passed, reason, "", "")
	case p.Count == 1:
		if reason == "" {
			reason = "1 approval needed to merge"
		}
	default:
		if reason == "" {
			reason = "no approval needed to merge"
		}
	}
	return it.with(Warning, reason, whyApprovals, fixApprovals)
}

func skillMatchItem(it Item, ok bool) func(analysis) Item {
	return func(a analysis) Item {
		if !ok {
			return it
		}
		matching := 0
		for _, f := range a.effective {
			if f.Covers() {
				matching++
			}
		}
		switch {
		case a.covers:
			return it.with(Passed, fmt.Sprintf("%d of %d turned-on skills match files", matching, len(a.effective)), "", "")
		case len(a.effective) == 0:
			return it.with(Missing, "no skills are turned on for this repository", whySkillMatch, fixSkillMatch)
		case a.notScanned:
			return it.with(Unknown, "a library skill was added after the last scan: scan again to see whether it matches", "", "")
		}
		return it.with(Missing, "no turned-on skill matches a file", whySkillMatch, fixSkillMatch)
	}
}

func unmatchedItem(it Item, ok bool) func(analysis) Item {
	return func(a analysis) Item {
		if !ok {
			return it
		}
		var list []string
		for _, f := range a.effective {
			switch {
			case f.Problem == ProblemNotScanned || f.Covers():
				// Not a gap: the scan has not seen it, or it covers.
			case f.Problem != "":
				list = append(list, f.Name+": "+f.Problem)
			default:
				list = append(list, f.Name)
			}
		}
		if len(list) == 0 {
			return it.with(Passed, "every turned-on skill matches a file", "", "")
		}
		it = it.with(Warning, fmt.Sprintf("%d turned-on skills match nothing", len(list)), whyUnmatched, fixUnmatched)
		it.List = list
		return it
	}
}

func languagesItem(it Item, ok bool) func(*JobResult) Item {
	return func(job *JobResult) Item {
		if !ok {
			return it
		}
		var list []string
		for _, l := range job.Languages {
			if l.Uncovered {
				list = append(list, fmt.Sprintf("%s (%d files)", l.Name, l.Files))
			}
		}
		if len(list) == 0 {
			return it.with(Passed, "every common language has a skill", "", "")
		}
		it = it.with(Warning, fmt.Sprintf("%d languages have no skill", len(list)), whyLanguages, fixLanguages)
		it.List = list
		return it
	}
}

func noiseItem(it Item, ok bool) func(*JobResult) Item {
	return func(job *JobResult) Item {
		if !ok {
			return it
		}
		total := max(job.NoiseTotal, len(job.Noise))
		if total == 0 {
			return it.with(Passed, "no generated, vendored or very large files a review would read", "", "")
		}
		it = it.with(Warning, fmt.Sprintf("%d files a review reads look generated, vendored or very large", total), whyNoise, noiseFix(job.Noise))
		it.Noise = slices.Clone(job.Noise)
		it.More = max(total-len(job.Noise), 0)
		return it
	}
}

func budgetItem(it Item, ok bool) func(analysis) Item {
	return func(a analysis) Item {
		if !ok {
			return it
		}
		if !a.flags.BudgetKnown {
			return it.with(Unknown, a.sizeWhy, "", "")
		}
		reason := fmt.Sprintf("%d KiB of %d KiB", kib(a.flags.Budget), inputs.ConfigMapLimit/1024)
		if a.flags.Budget > inputs.ConfigMapLimit {
			return it.with(Missing, reason, whyBudget, fixBudget)
		}
		return it.with(Passed, reason, "", "")
	}
}

// kib is bytes in KiB, rounded up.
func kib(n int) int { return (n + 1023) / 1024 }

// suggested lists the library skills whose paths match files and that are not
// turned on, most files first. A skill with no paths is never suggested: it
// would match every repository. A repository skill of the same name replaces
// a library one, so that is not suggested either.
func suggested(job *JobResult, effective []SkillFit) []string {
	on := map[string]bool{}
	for _, f := range effective {
		on[f.Name] = true
	}
	var fits []SkillFit
	for _, f := range job.Skills {
		if f.Folder == inputs.SourceLibrary && f.Problem == "" && f.HasPaths && f.Files > 0 && !on[f.Name] {
			fits = append(fits, f)
		}
	}
	sort.SliceStable(fits, func(i, j int) bool {
		return fits[i].Files > fits[j].Files || (fits[i].Files == fits[j].Files && fits[i].Name < fits[j].Name)
	})
	var out []string
	for _, f := range fits {
		out = append(out, f.Name)
	}
	return out
}

// noiseFix is the lines to copy: one .gitattributes line per distinct pattern
// (linguist-generated is what a review honours; linguist-vendored it ignores),
// and per reviewing skill a block to paste in its frontmatter. A path that a
// .gitattributes line can't name as the review reads it gets the skill
// block only.
func noiseFix(noise []NoiseFile) string {
	var patterns []string
	seen := map[string]bool{}
	perSkill := map[string][]string{}
	for _, n := range noise {
		if p, ok := noisePattern(n); ok && !seen[p] {
			seen[p] = true
			patterns = append(patterns, p)
		}
		for _, s := range n.Skills {
			perSkill[s] = append(perSkill[s], globLiteral(n.Path))
		}
	}
	sort.Strings(patterns)

	var b strings.Builder
	if len(patterns) > 0 {
		b.WriteString("Add to .gitattributes:\n")
		for _, p := range patterns {
			b.WriteString(p + " linguist-generated\n")
		}
		if len(perSkill) > 0 {
			b.WriteString("Or:\n")
		}
	}
	names := make([]string, 0, len(perSkill))
	for s := range perSkill {
		names = append(names, s)
	}
	sort.Strings(names)
	for _, s := range names {
		paths := perSkill[s]
		sort.Strings(paths)
		fmt.Fprintf(&b, "In skill %s's frontmatter, add to exclude:\n%s", s, excludeBlock(paths))
	}
	return strings.TrimRight(b.String(), "\n")
}

// noisePattern is the .gitattributes pattern for a file: its build output
// directory (dist or build), else its path. ok is false when the pattern
// can't be written so that the review reads it as written: the review splits
// a line on whitespace, does no unquoting, reads glob characters, and a line
// starting with # or ! is no pattern to it.
func noisePattern(n NoiseFile) (string, bool) {
	p := n.Path
	glob := ""
	if n.Why == WhyBuildOutput {
		segs := strings.Split(n.Path, "/")
		for i, s := range segs[:len(segs)-1] {
			if s == "dist" || s == "build" {
				p, glob = strings.Join(segs[:i+1], "/"), "/**"
				break
			}
		}
	}
	if p == "" || strings.HasPrefix(p, "#") || strings.HasPrefix(p, "!") ||
		strings.ContainsAny(p, "\"\\[*?") ||
		strings.ContainsFunc(p, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return "", false
	}
	return p + glob, true
}

// globLiteral is a path as a pattern that matches only that path: the glob
// characters a review's matcher reads (\ * ? [) are escaped.
func globLiteral(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\*?[`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// excludeBlock is the YAML to paste into a skill's frontmatter: exclude:
// then one "  - <path>" line per path, quoted where YAML needs it.
func excludeBlock(paths []string) string {
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(map[string][]string{"exclude": paths}); err != nil {
		// A list of strings always encodes; this is never reached.
		return ""
	}
	_ = enc.Close()
	return b.String()
}

package readiness

import (
	"slices"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/inputs"
)

// goodJob is a scan that finds every file item in order.
func goodJob() *JobResult {
	return &JobResult{
		Version: ResultVersion,
		Commit:  "abc",
		Files:   10, SourceFiles: 10,
		Context: Context{AgentFile: "AGENTS.md", Readme: "README.md", Contributing: "CONTRIBUTING.md", CodeOwners: "CODEOWNERS", CodeOwnerRules: 2, Instructions: true},
		Skills:  []SkillFit{{Name: "go", Folder: inputs.SourceLibrary, HasPaths: true, Files: 4}},
	}
}

func goodPod() *PodResult {
	return &PodResult{
		Version:        1,
		Approvals:      PodItem{State: Passed, Count: 2},
		Protected:      PodItem{State: Passed},
		RequiredChecks: PodItem{State: Passed},
	}
}

func goodInput() ChecklistInput {
	return ChecklistInput{Pod: goodPod(), Job: goodJob(), Library: []string{"go"}, LibrarySizes: map[string]int{"go": 1000}}
}

func items(c Checklist) map[string]Item {
	out := map[string]Item{}
	for _, s := range c.Sections {
		for _, it := range s.Items {
			out[it.Key] = it
		}
	}
	return out
}

func TestChecklistOrderAndKeys(t *testing.T) {
	c := BuildChecklist(goodInput())
	want := map[string][]string{
		"context": {"agent_file", "readme", "contributing", "instructions"},
		"process": {"codeowners", "approvals", "protected", "required_checks"},
		"skills":  {"skill_match", "skills_unmatched", "languages"},
		"size":    {"noise", "budget"},
	}
	var order []string
	for _, s := range c.Sections {
		order = append(order, s.Key)
		var keys []string
		for _, it := range s.Items {
			keys = append(keys, it.Key)
		}
		if !slices.Equal(keys, want[s.Key]) {
			t.Errorf("section %s items = %v, want %v", s.Key, keys, want[s.Key])
		}
	}
	if !slices.Equal(order, []string{"context", "process", "skills", "size"}) {
		t.Errorf("sections = %v", order)
	}
	for key, it := range items(c) {
		if it.State != Passed {
			t.Errorf("%s = %s (%s), want passed on a good scan", key, it.State, it.Reason)
		}
	}
	if c.Budget != 1000 {
		t.Errorf("Budget = %d, want 1000", c.Budget)
	}
}

func TestChecklistFindings(t *testing.T) {
	in := goodInput()
	in.Job.Context = Context{AgentFile: "AGENTS.md", AgentFileEmpty: true, CodeOwners: "CODEOWNERS"}
	got := items(BuildChecklist(in))
	for key, want := range map[string]string{
		"agent_file":   Missing, // an empty file gives a review nothing
		"readme":       Missing,
		"contributing": Warning,
		"instructions": Warning,
		"codeowners":   Warning, // present with no rule
	} {
		if got[key].State != want {
			t.Errorf("%s = %s (%s), want %s", key, got[key].State, got[key].Reason, want)
		}
		if got[key].Why == "" || got[key].Fix == "" {
			t.Errorf("%s has no why or fix", key)
		}
	}
	if got["agent_file"].Reason == "" {
		t.Error("agent_file has no reason")
	}

	in.Job.Context = goodJob().Context
	in.Job.Context.CodeOwners = ""
	if got := items(BuildChecklist(in))["codeowners"]; got.State != Warning {
		t.Errorf("no CODEOWNERS = %s, want warning", got.State)
	}
}

func TestChecklistPodItems(t *testing.T) {
	in := goodInput()
	in.Pod = &PodResult{
		Approvals:      PodItem{State: Passed, Count: 1},
		Protected:      PodItem{State: Missing, Reason: "the default branch is not protected"},
		RequiredChecks: PodItem{State: Warning, Reason: "pipelines need not succeed"},
	}
	got := items(BuildChecklist(in))
	if got["approvals"].State != Warning {
		t.Errorf("1 approval = %s, want warning whatever the pod state", got["approvals"].State)
	}
	if got["protected"].State != Missing || got["protected"].Reason != "the default branch is not protected" {
		t.Errorf("protected = %+v", got["protected"])
	}
	if got["required_checks"].State != Warning {
		t.Errorf("required_checks = %s", got["required_checks"].State)
	}
	in.Pod.Approvals = PodItem{State: Passed, Count: 2}
	if got := items(BuildChecklist(in))["approvals"]; got.State != Passed {
		t.Errorf("2 approvals = %s, want passed", got.State)
	}
}

func TestChecklistUnknownNeverPassed(t *testing.T) {
	unknown := PodItem{State: Unknown, Reason: "the bot can't read it"}
	podKeys := []string{"approvals", "protected", "required_checks"}
	fileKeys := []string{"agent_file", "readme", "contributing", "instructions", "codeowners", "skill_match", "skills_unmatched", "languages", "noise", "budget"}

	cases := []struct {
		name string
		edit func(*ChecklistInput)
		keys []string
	}{
		{"pod nil", func(in *ChecklistInput) { in.Pod = nil }, podKeys},
		{"job nil", func(in *ChecklistInput) { in.Job = nil; in.FileError = "x" }, fileKeys},
		{"job nil without error", func(in *ChecklistInput) { in.Job = nil }, fileKeys},
		{"job with error", func(in *ChecklistInput) { in.Job = &JobResult{Version: 1, Error: "clone failed"} }, fileKeys},
		{"approvals unknown", func(in *ChecklistInput) { in.Pod.Approvals = unknown }, []string{"approvals"}},
		{"protected unknown", func(in *ChecklistInput) { in.Pod.Protected = unknown }, []string{"protected"}},
		{"checks unknown", func(in *ChecklistInput) { in.Pod.RequiredChecks = unknown }, []string{"required_checks"}},
		{"empty pod state", func(in *ChecklistInput) { in.Pod.Protected = PodItem{} }, []string{"protected"}},
		{"unlisted pod state", func(in *ChecklistInput) { in.Pod.Protected = PodItem{State: "ok"} }, []string{"protected"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := goodInput()
			c.edit(&in)
			got := items(BuildChecklist(in))
			for _, key := range c.keys {
				if got[key].State != Unknown {
					t.Errorf("%s = %s, want unknown", key, got[key].State)
				}
				if got[key].Reason == "" {
					t.Errorf("%s has no reason", key)
				}
			}
			for key, it := range got {
				if it.State == Passed && slices.Contains(c.keys, key) {
					t.Errorf("%s passed", key)
				}
			}
		})
	}
}

func TestChecklistFilesFailedKeepsSettings(t *testing.T) {
	in := goodInput()
	in.Job = nil
	in.FileError = "x"
	in.Pod = &PodResult{
		Approvals:      PodItem{State: Passed, Count: 3},
		Protected:      PodItem{State: Missing, Reason: "not protected"},
		RequiredChecks: PodItem{State: Unknown, Reason: "no permission"},
	}
	c := BuildChecklist(in)
	got := items(c)
	for _, key := range []string{"agent_file", "readme", "contributing", "instructions", "codeowners", "skill_match", "skills_unmatched", "languages", "noise", "budget"} {
		if got[key].State != Unknown || got[key].Reason != "couldn't scan files: x" {
			t.Errorf("%s = %s %q", key, got[key].State, got[key].Reason)
		}
	}
	if got["approvals"].State != Passed || got["protected"].State != Missing || got["required_checks"].State != Unknown {
		t.Errorf("pod items = %s %s %s", got["approvals"].State, got["protected"].State, got["required_checks"].State)
	}
	if len(c.Suggested) != 0 || c.Budget != 0 {
		t.Errorf("suggested %v budget %d without a file scan", c.Suggested, c.Budget)
	}
}

func TestChecklistLiveSingleApproval(t *testing.T) {
	in := goodInput()
	in.Pod.Approvals = PodItem{State: Passed, Count: 3}
	in.SingleApproval = true
	if got := items(BuildChecklist(in))["approvals"]; got.State != Warning {
		t.Errorf("approvals = %s, want warning when the hourly check says one is enough", got.State)
	}
	in.Pod = nil
	if got := items(BuildChecklist(in))["approvals"]; got.State != Warning {
		t.Errorf("approvals with no pod read = %s, want warning", got.State)
	}
}

func TestChecklistSuggested(t *testing.T) {
	in := goodInput()
	in.Library = []string{"go"}
	in.Job.Skills = []SkillFit{
		{Name: "go", Folder: inputs.SourceLibrary, HasPaths: true, Files: 4},
		{Name: "py", Folder: inputs.SourceLibrary, HasPaths: true, Files: 9},
		{Name: "rust", Folder: inputs.SourceLibrary, HasPaths: true, Files: 0},
		{Name: "all", Folder: inputs.SourceLibrary}, // no paths: would match every repository
		{Name: "ts", Folder: inputs.SourceLibrary, HasPaths: true, Files: 2},
		{Name: "bad", Folder: inputs.SourceLibrary, HasPaths: true, Files: 3, Problem: "broken"},
	}
	got := BuildChecklist(in).Suggested
	if !slices.Equal(got, []string{"py", "ts"}) {
		t.Errorf("Suggested = %v, want [py ts] (assigned, no files, no paths and broken skills are not suggested)", got)
	}
}

func TestChecklistSuggestedSkipsShadowedLibrarySkill(t *testing.T) {
	in := goodInput()
	in.Library = nil
	in.RepoSkills = []string{"py"}
	in.Job.Skills = []SkillFit{
		{Name: "py", Folder: inputs.RepoFolders[1], HasPaths: true, Files: 3},
		{Name: "py", Folder: inputs.SourceLibrary, HasPaths: true, Files: 3},
	}
	if got := BuildChecklist(in).Suggested; len(got) != 0 {
		t.Errorf("Suggested = %v: a repository skill of that name replaces the library one", got)
	}
}

func TestChecklistNoiseFix(t *testing.T) {
	in := goodInput()
	in.Job.NoiseTotal = 5
	in.Job.Noise = []NoiseFile{
		{Path: "dist/a.js", Why: WhyBuildOutput, Size: 40, Skills: []string{"go"}},
		{Path: "web/dist/b.js", Why: WhyBuildOutput, Size: 30, Skills: []string{"go", "ts"}},
		{Path: "dist/c.js", Why: WhyBuildOutput, Size: 20, Skills: []string{"go"}},
		{Path: "api/x.pb.go", Why: WhyGeneratedName, Size: 10},
		{Path: "data/big file.json", Why: WhyLarge, Size: 600 << 10},
	}
	got := items(BuildChecklist(in))["noise"]
	if got.State != Warning {
		t.Fatalf("noise = %s", got.State)
	}
	for _, want := range []string{"dist/** linguist-generated", "web/dist/** linguist-generated", "api/x.pb.go linguist-generated", `"data/big file.json" linguist-generated`} {
		if !strings.Contains(got.Fix, want) {
			t.Errorf("Fix lacks %q:\n%s", want, got.Fix)
		}
	}
	if n := strings.Count(got.Fix, "dist/** linguist-generated"); n != 2 { // "dist/**" and "web/dist/**"
		t.Errorf("dist/** pattern lines = %d, want 1 per distinct pattern (2 with web/dist):\n%s", n, got.Fix)
	}
	if strings.Count(got.Fix, "\ndist/** linguist-generated") != 1 {
		t.Errorf("dist/** is repeated:\n%s", got.Fix)
	}
	if strings.Contains(got.Fix, "linguist-vendored") {
		t.Errorf("Fix suggests linguist-vendored, which a review ignores:\n%s", got.Fix)
	}
	if !strings.Contains(got.Fix, "exclude: [dist/a.js, dist/c.js, web/dist/b.js]") {
		t.Errorf("Fix lacks go's exclude line:\n%s", got.Fix)
	}
	if !strings.Contains(got.Fix, "exclude: [web/dist/b.js]") {
		t.Errorf("Fix lacks ts's exclude line:\n%s", got.Fix)
	}
	if len(got.Noise) != 5 || got.More != 0 {
		t.Errorf("Noise %d More %d", len(got.Noise), got.More)
	}

	in.Job.NoiseTotal = 70
	if got := items(BuildChecklist(in))["noise"]; got.More != 65 {
		t.Errorf("More = %d, want 65 (70 found, 5 listed)", got.More)
	}

	in.Job.Noise, in.Job.NoiseTotal = nil, 0
	if got := items(BuildChecklist(in))["noise"]; got.State != Passed {
		t.Errorf("no noise = %s", got.State)
	}
}

func TestChecklistSkillItems(t *testing.T) {
	in := goodInput()
	in.Library = []string{"go", "py", "gone"}
	in.RepoSkills = []string{"mine", "lost"}
	in.Job.Skills = []SkillFit{
		{Name: "go", Folder: inputs.SourceLibrary, HasPaths: true, Files: 4},
		{Name: "py", Folder: inputs.SourceLibrary, HasPaths: true},
		{Name: "mine", Folder: inputs.RepoFolders[1], HasPaths: true, Files: 1, Bytes: 10},
	}
	in.Job.Languages = []Language{{Name: "Go", Files: 8}, {Name: "Rust", Files: 3, Uncovered: true}}
	got := items(BuildChecklist(in))

	if got["skill_match"].State != Passed {
		t.Errorf("skill_match = %s", got["skill_match"].State)
	}
	un := got["skills_unmatched"]
	if un.State != Warning || len(un.List) != 2 {
		t.Fatalf("skills_unmatched = %s %v", un.State, un.List)
	}
	// Effective order: repository skills first, then the library.
	if want := (&inputs.NotFoundError{Name: "lost"}).Error(); !strings.Contains(un.List[0], "lost") || !strings.Contains(un.List[0], want) {
		t.Errorf("a skill with a Problem shows its text: %q", un.List[0])
	}
	if un.List[1] != "py" {
		t.Errorf("List[1] = %q", un.List[1])
	}
	for _, e := range un.List {
		if strings.Contains(e, "gone") {
			t.Errorf("a skill the scan has not seen is no gap: %v", un.List)
		}
	}
	lang := got["languages"]
	if lang.State != Warning || len(lang.List) != 1 || !strings.Contains(lang.List[0], "Rust") {
		t.Errorf("languages = %s %v", lang.State, lang.List)
	}
}

func TestChecklistSkillMatchMissing(t *testing.T) {
	in := goodInput()
	in.Job.Skills = []SkillFit{{Name: "go", Folder: inputs.SourceLibrary, HasPaths: true}}
	if got := items(BuildChecklist(in))["skill_match"]; got.State != Missing || got.Why == "" || got.Fix == "" {
		t.Errorf("no skill matches = %+v", got)
	}
	in.Library = nil
	if got := items(BuildChecklist(in))["skill_match"]; got.State != Missing {
		t.Errorf("no skill assigned = %s, want missing", got.State)
	}
}

func TestNotScannedSkillIsNoGap(t *testing.T) {
	in := goodInput()
	in.Library = []string{"old", "new"}
	in.LibrarySizes = map[string]int{"old": 100, "new": 300}
	in.Job.InstructionsBytes = 50
	in.Job.Skills = []SkillFit{{Name: "old", Folder: inputs.SourceLibrary, HasPaths: true}} // matches nothing; "new" was added after the scan

	fl := ComputeFlags(in.Job, in.Library, in.RepoSkills, in.LibrarySizes)
	if fl.NoCoverage {
		t.Error("NoCoverage raised on a guess: a skill the scan has not seen may cover")
	}
	if !fl.BudgetKnown || fl.Budget != 450 {
		t.Errorf("budget %d known %v, want 450 known: the size of a skill not scanned comes from the library", fl.Budget, fl.BudgetKnown)
	}

	got := items(BuildChecklist(in))
	m := got["skill_match"]
	if m.State != Unknown || !strings.Contains(strings.ToLower(m.Reason), "scan again") {
		t.Errorf("skill_match = %s %q, want unknown telling the user to scan again", m.State, m.Reason)
	}
	un := got["skills_unmatched"]
	if un.State != Warning || !slices.Equal(un.List, []string{"old"}) {
		t.Errorf("skills_unmatched = %s %v, want only old", un.State, un.List)
	}
	if got["budget"].State != Passed {
		t.Errorf("budget = %s (%s)", got["budget"].State, got["budget"].Reason)
	}

	// When a seen skill covers, the one not scanned changes nothing.
	in.Job.Skills[0].Files = 2
	if got := items(BuildChecklist(in))["skill_match"]; got.State != Passed {
		t.Errorf("skill_match = %s", got.State)
	}

	// Alone and unseen: no match is known, so no list entry either.
	in.Library = []string{"new"}
	in.Job.Skills = nil
	got = items(BuildChecklist(in))
	if got["skill_match"].State != Unknown || got["skills_unmatched"].State != Passed {
		t.Errorf("only an unseen skill: skill_match %s, skills_unmatched %s", got["skill_match"].State, got["skills_unmatched"].State)
	}
}

func TestFlagsNoPathsSkillCovers(t *testing.T) {
	job := goodJob()
	job.Skills = []SkillFit{{Name: "all", Folder: inputs.SourceLibrary, Files: 0}}
	if fl := ComputeFlags(job, []string{"all"}, nil, map[string]int{"all": 1}); fl.NoCoverage {
		t.Error("an assigned skill with no paths covers every repository")
	}
}

func TestFlagsZeroAssigned(t *testing.T) {
	job := goodJob()
	job.Skills = []SkillFit{{Name: "go", Folder: inputs.SourceLibrary, HasPaths: true, Files: 3}}
	if fl := ComputeFlags(job, nil, nil, nil); fl.NoCoverage {
		t.Error("no effective skill: no_skills is the review's item, not coverage")
	}
	if fl := ComputeFlags(job, []string{"go"}, nil, map[string]int{"go": 1}); fl.NoCoverage {
		t.Error("a covering skill raised NoCoverage")
	}
	job.Skills[0].Files = 0
	if fl := ComputeFlags(job, []string{"go"}, nil, map[string]int{"go": 1}); !fl.NoCoverage {
		t.Error("an assigned skill that matches no file should raise NoCoverage")
	}
}

func TestFlagsNoContext(t *testing.T) {
	job := goodJob()
	job.Context = Context{}
	if fl := ComputeFlags(job, nil, nil, nil); !fl.NoContext {
		t.Error("no agent file and no README should raise NoContext")
	}
	job.Context = Context{Readme: "README.md"}
	if fl := ComputeFlags(job, nil, nil, nil); fl.NoContext {
		t.Error("a README is context")
	}
	job.Context = Context{AgentFile: "CLAUDE.md"}
	if fl := ComputeFlags(job, nil, nil, nil); fl.NoContext {
		t.Error("an agent file is context")
	}
	job.Context = Context{AgentFile: "AGENTS.md", AgentFileEmpty: true}
	if fl := ComputeFlags(job, nil, nil, nil); !fl.NoContext {
		t.Error("an empty agent file is no context")
	}
}

func TestFlagsNoResult(t *testing.T) {
	for name, job := range map[string]*JobResult{"nil": nil, "error": {Version: 1, Error: "boom"}} {
		fl := ComputeFlags(job, []string{"go"}, nil, map[string]int{"go": 1})
		if fl != (Flags{}) {
			t.Errorf("%s: flags = %+v, want none: nothing was scanned", name, fl)
		}
	}
}

func TestFlagsBudget(t *testing.T) {
	job := goodJob()
	job.InstructionsBytes = 100
	job.Skills = []SkillFit{
		{Name: "go", Folder: inputs.SourceLibrary, HasPaths: true, Files: 3},
		{Name: "mine", Folder: inputs.RepoFolders[0], Bytes: 20},
		{Name: "other", Folder: inputs.RepoFolders[2], Bytes: 7, HasPaths: true, Files: 1},
		{Name: "unused", Folder: inputs.RepoFolders[2], Bytes: 5000}, // not turned on
		{Name: "idle", Folder: inputs.SourceLibrary, Files: 9},       // not assigned
	}
	lib := map[string]int{"go": 1000, "idle": 99999}
	fl := ComputeFlags(job, []string{"go"}, []string{"other"}, lib)
	if !fl.BudgetKnown || fl.Budget != 100+20+7+1000 {
		t.Errorf("budget %d known %v, want %d known", fl.Budget, fl.BudgetKnown, 100+20+7+1000)
	}

	// An effective repository skill with a Problem: the size is unknown.
	fl = ComputeFlags(job, []string{"go"}, []string{"other", "lost"}, lib)
	if fl.BudgetKnown {
		t.Error("BudgetKnown with a repository skill that was not found")
	}
	job.Skills[2].Problem = "malformed"
	fl = ComputeFlags(job, []string{"go"}, []string{"other"}, lib)
	if fl.BudgetKnown {
		t.Error("BudgetKnown with a malformed repository skill")
	}
}

func TestChecklistBudget(t *testing.T) {
	in := goodInput()
	in.Library = []string{"go"}
	in.LibrarySizes = map[string]int{"go": inputs.ConfigMapLimit}
	if got := items(BuildChecklist(in))["budget"]; got.State != Passed {
		t.Errorf("exactly the limit = %s", got.State)
	}
	in.Job.InstructionsBytes = 1
	got := items(BuildChecklist(in))["budget"]
	if got.State != Missing || got.Why == "" || got.Fix == "" || !strings.Contains(got.Reason, "900 KiB") {
		t.Errorf("over the limit = %+v", got)
	}

	in.Job.Skills = []SkillFit{{Name: "go", Folder: inputs.SourceLibrary, HasPaths: true, Files: 1}}
	in.RepoSkills = []string{"lost"}
	got = items(BuildChecklist(in))["budget"]
	if got.State != Unknown || !strings.Contains(got.Reason, "lost") {
		t.Errorf("a skill with a Problem = %s %q, want unknown naming it", got.State, got.Reason)
	}
}

func TestChecklistPassedHasNoAdvice(t *testing.T) {
	for key, it := range items(BuildChecklist(goodInput())) {
		if it.Why != "" || it.Fix != "" {
			t.Errorf("%s passed but carries advice", key)
		}
	}
}

func TestFlagsBudgetUnknownLibrarySize(t *testing.T) {
	job := goodJob()
	job.Skills = []SkillFit{{Name: "go", Folder: inputs.SourceLibrary, HasPaths: true, Files: 3}}

	// "gone" is on the repository's list but was deleted from the library.
	lib := map[string]int{"go": 1000}
	fl := ComputeFlags(job, []string{"go", "gone"}, nil, lib)
	if fl.BudgetKnown {
		t.Error("BudgetKnown with a library skill whose size is not known")
	}
	in := goodInput()
	in.Job = job
	in.Library = []string{"go", "gone"}
	in.LibrarySizes = lib
	got := items(BuildChecklist(in))["budget"]
	if got.State != Unknown || !strings.Contains(got.Reason, "gone") {
		t.Errorf("budget = %s %q, want unknown naming the skill", got.State, got.Reason)
	}

	// A size of 0 is a known size; a skill the scan has not seen but the
	// library lists keeps the size known.
	lib = map[string]int{"go": 1000, "empty": 0, "new": 5}
	fl = ComputeFlags(job, []string{"go", "empty", "new"}, nil, lib)
	if !fl.BudgetKnown || fl.Budget != job.InstructionsBytes+1005 {
		t.Errorf("budget %d known %v, want %d known", fl.Budget, fl.BudgetKnown, job.InstructionsBytes+1005)
	}
}

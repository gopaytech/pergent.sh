package kube

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

var updateNoLinks = flag.Bool("update-no-links", false, "rewrite testdata/job-no-links.golden.json (only when a Job changes on purpose for every review)")

// noLinksGolden is the Job and ConfigMap of a review with no linked
// repository, as JSON. The checkout script is a placeholder: its own text may
// change, and its behaviour with no links is pinned by the script tests.
func noLinksGolden(t *testing.T, p JobParams) string {
	t.Helper()
	job := BuildJob(p)
	script := job.Spec.Template.Spec.InitContainers[0].Command[2]
	b, err := json.MarshalIndent(map[string]any{"job": job, "configMap": BuildConfigMap(p, nil)}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), jsonString(t, script), `"<checkout.sh>"`) + "\n"
}

func jsonString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// With the setting off, or on and nothing matched, a review's Job, ConfigMap
// and environment are what they were before linked repositories existed.
func TestJobUnchangedWithoutLinks(t *testing.T) {
	p := testParams()
	p.WorkSize = resource.MustParse("20Gi")
	got := noLinksGolden(t, p)
	if !strings.Contains(got, `"<checkout.sh>"`) {
		t.Fatal("the checkout script was not replaced in the golden text")
	}
	const path = "testdata/job-no-links.golden.json"
	if *updateNoLinks {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("a review without links changed:\n got:\n%s\nwant:\n%s", got, want)
	}

	// Empty is none, however it is spelled.
	q := testParams()
	q.WorkSize = resource.MustParse("20Gi")
	q.LinkedClones = []LinkedClone{}
	q.Inputs.Linked = []byte{}
	if noLinksGolden(t, q) != got {
		t.Error("an empty list of clones, or empty linked-repos.json, changed the Job")
	}
}

func linkedParams() JobParams {
	p := testParams()
	p.WorkSize = resource.MustParse("20Gi")
	p.LinkedClones = []LinkedClone{
		{Dir: "linked-1", CloneURL: "https://github.com/acme/api.git"},
		{Dir: "linked-2", CloneURL: "https://github.com/acme/web.git"},
	}
	p.Inputs.Linked = []byte(`[{"name":"acme/api","path":"/work/linked-1","watched":["render/**"],"can_block":false,"error":""}]`)
	return p
}

func envValue(env []corev1.EnvVar, name string) (string, bool) {
	for _, e := range env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

func TestJobClonesMatchedTargets(t *testing.T) {
	p := linkedParams()
	job := BuildJob(p)
	pod := job.Spec.Template.Spec

	checkout := containerByName(t, pod.InitContainers, "checkout")
	if got, _ := envValue(checkout.Env, "LINKED_CLONES"); got != "linked-1 https://github.com/acme/api.git\nlinked-2 https://github.com/acme/web.git" {
		t.Errorf("LINKED_CLONES = %q", got)
	}
	main := containerByName(t, pod.Containers, "pergent")
	if got, _ := envValue(main.Env, "PERGENT_LINKED_REPOS"); got != "/in/linked-repos.json" {
		t.Errorf("PERGENT_LINKED_REPOS = %q", got)
	}
	// An environment variable, not a flag: an older image exits 2 on a flag.
	for _, a := range main.Args {
		if strings.Contains(a, "linked") {
			t.Errorf("unexpected arg %q", a)
		}
	}
	if _, ok := envValue(checkout.Env, "PERGENT_LINKED_REPOS"); ok {
		t.Error("the checkout container has PERGENT_LINKED_REPOS")
	}
	if got := BuildConfigMap(p, nil).Data["linked-repos.json"]; got != string(p.Inputs.Linked) {
		t.Errorf("linked-repos.json = %q", got)
	}
	var item bool
	for _, v := range pod.Volumes {
		if v.ConfigMap == nil {
			continue
		}
		for _, it := range v.ConfigMap.Items {
			if it.Key == "linked-repos.json" && it.Path == "linked-repos.json" {
				item = true
			}
		}
	}
	if !item {
		t.Error("no linked-repos.json item in the inputs volume")
	}

	// Each target may take 2 GiB, so a limited work volume grows by that.
	for _, v := range pod.Volumes {
		if v.Name == "work" && (v.EmptyDir.SizeLimit == nil || v.EmptyDir.SizeLimit.String() != "24Gi") {
			t.Errorf("work volume = %+v; want 24Gi (20Gi and two clones)", v.EmptyDir)
		}
	}
	// An unlimited volume stays unlimited.
	p.WorkSize = resource.Quantity{}
	for _, v := range BuildJob(p).Spec.Template.Spec.Volumes {
		if v.Name == "work" && v.EmptyDir.SizeLimit != nil {
			t.Errorf("an unlimited work volume got a limit: %v", v.EmptyDir.SizeLimit)
		}
	}
	// The pergent container still cannot reach the clone token.
	for _, e := range main.Env {
		if e.Name == "GIT_TOKEN" {
			t.Error("the pergent container has GIT_TOKEN")
		}
	}
}

func TestCloneAllowanceCountsTargets(t *testing.T) {
	for _, c := range []struct {
		secs, targets int
		want          time.Duration
	}{
		{0, 0, 2 * time.Minute},
		{0, 1, 4 * time.Minute},
		{0, 3, 8 * time.Minute},
		{90, 2, 3*time.Minute + 4*time.Minute},
	} {
		if got := CloneAllowance(c.secs, c.targets); got != c.want {
			t.Errorf("CloneAllowance(%d, %d) = %v, want %v", c.secs, c.targets, got, c.want)
		}
	}
	// The script's bound and the allowance are one number.
	if !strings.Contains(checkoutScript, "timeout "+itoa(int(LinkedCloneTimeout/time.Second))+" git clone") {
		t.Errorf("the script's timeout is not LinkedCloneTimeout (%v)", LinkedCloneTimeout)
	}
	if !strings.Contains(checkoutScript, "-le "+itoa(LinkedCloneMaxKiB)) {
		t.Errorf("the script's size limit is not LinkedCloneMaxKiB (%d)", LinkedCloneMaxKiB)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestLaunchRefusesCredentialCloneURL(t *testing.T) {
	for _, u := range []string{"https://x-access-token:ghs_abc@github.com/acme/api.git", "https://tok@github.com/acme/api.git", "http://github.com/acme/api.git", "git@github.com:acme/api.git", "file:///etc"} {
		p := linkedParams()
		p.LinkedClones[0].CloneURL = u
		if err := checkLinkedClones(p.LinkedClones); err == nil {
			t.Errorf("checkLinkedClones(%q) = nil", u)
		}
	}
	if err := checkLinkedClones(linkedParams().LinkedClones); err != nil {
		t.Errorf("checkLinkedClones = %v", err)
	}
	p := linkedParams()
	p.LinkedClones[0].Dir = "../escape"
	if err := checkLinkedClones(p.LinkedClones); err == nil {
		t.Error("a directory outside linked-N was accepted")
	}
}

// ---------------------------------------------------------------------------
// The checkout script, run for real with a fake git on PATH.

const secretToken = "tok-secret-value-0123"

type scriptRun struct {
	work string
	log  []string
	err  error
	out  string
}

// has reports whether some logged line contains every part.
func (r scriptRun) has(parts ...string) bool {
	for _, l := range r.log {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(l, p)
		}
		if ok {
			return true
		}
	}
	return false
}

func (r scriptRun) exists(name string) bool {
	_, err := os.Stat(filepath.Join(r.work, name))
	return err == nil
}

func writeExec(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// runCheckout runs checkout.sh with a work directory of its own (the script's
// /work is rewritten) and fake git, timeout, du and base64: the real ones need
// a network, GNU options or both. A clone of a URL with "broken" in it fails
// after leaving a partial directory, one with "slow" times out, one with
// "huge" is one KiB over the size limit, one with "exact" is exactly at it.
// fetchFails makes the checkout itself fail.
func runCheckout(t *testing.T, clones string, fetchFails bool) scriptRun {
	t.Helper()
	root := t.TempDir()
	work, bin, logFile := filepath.Join(root, "work"), filepath.Join(root, "bin"), filepath.Join(root, "log")
	for _, d := range []string{work, bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExec(t, bin, "git", `echo "git $*" >> "$FAKE_LOG"
case "$1" in
init) mkdir -p "$3" ;;
fetch) [ -z "$FAKE_FETCH_FAILS" ] || exit 1 ;;
clone)
  prev=; last=
  for a in "$@"; do prev="$last"; last="$a"; done
  echo "clone-env allow=${GIT_ALLOW_PROTOCOL-unset} prompt=${GIT_TERMINAL_PROMPT-unset} nosystem=${GIT_CONFIG_NOSYSTEM-unset} global=${GIT_CONFIG_GLOBAL-unset} header=${GIT_CONFIG_VALUE_0:+set}" >> "$FAKE_LOG"
  mkdir -p "$last/.git"
  printf '[remote "origin"]\n\turl = %s\n' "$prev" > "$last/.git/config"
  case "$prev" in *huge*) echo 2097153 > "$last/size.kb" ;; *exact*) echo 2097152 > "$last/size.kb" ;; *) echo 10 > "$last/size.kb" ;; esac
  case "$prev" in *broken*) exit 128 ;; esac ;;
esac
exit 0
`)
	writeExec(t, bin, "timeout", `echo "timeout $*" >> "$FAKE_LOG"
shift
case "$*" in *slow*) exit 124 ;; esac
exec "$@"
`)
	writeExec(t, bin, "du", `printf '%s\t%s\n' "$(cat "$2/size.kb")" "$2"
`)
	writeExec(t, bin, "base64", "cat > /dev/null; printf 'ZmFrZQ=='\n")

	script := strings.ReplaceAll(checkoutScript, "/work", work)
	shell := "sh"
	if p, err := exec.LookPath("dash"); err == nil {
		shell = p // the image's sh
	}
	cmd := exec.Command(shell, "-ec", script)
	cmd.Env = []string{
		"PATH=" + bin + ":/usr/bin:/bin", "FAKE_LOG=" + logFile,
		"REPO_URL=https://github.com/acme/widgets.git", "MR_REF=refs/pull/1/head", "TARGET_BRANCH=main",
		"BASE_SHA=" + testBase, "HEAD_SHA=" + testHead, "GIT_USERNAME=x-access-token", "GIT_TOKEN=" + secretToken,
	}
	if clones != "" {
		cmd.Env = append(cmd.Env, "LINKED_CLONES="+clones)
	}
	if fetchFails {
		cmd.Env = append(cmd.Env, "FAKE_FETCH_FAILS=1")
	}
	out, err := cmd.CombinedOutput()
	r := scriptRun{work: work, err: err, out: string(out)}
	if b, rerr := os.ReadFile(logFile); rerr == nil {
		r.log = strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	return r
}

const twoClones = "linked-1 https://github.com/acme/api.git\nlinked-2 https://github.com/acme/web.git"

// With no linked repositories the script does what it always did: the same
// git commands, no clone, no timeout.
func TestCheckoutScriptWithoutLinksIsUnchanged(t *testing.T) {
	r := runCheckout(t, "", false)
	if r.err != nil {
		t.Fatalf("script: %v\n%s", r.err, r.out)
	}
	want := []string{
		"git init -q " + r.work + "/repo",
		"git config core.hooksPath /dev/null",
		"git remote add origin https://github.com/acme/widgets.git",
		"git remote set-url --push origin pergent-push-disabled:",
		"git fetch -q --no-tags --depth=1 origin " + testBase + " " + testHead,
		"git checkout -q --detach " + testHead,
	}
	if strings.Join(r.log, "\n") != strings.Join(want, "\n") {
		t.Errorf("git calls:\n%s\nwant:\n%s", strings.Join(r.log, "\n"), strings.Join(want, "\n"))
	}
	entries, _ := os.ReadDir(r.work)
	if len(entries) != 1 || entries[0].Name() != "repo" {
		t.Errorf("work holds %v; want only repo", entries)
	}
}

// A failed checkout still fails the Job, and no target is cloned after it.
func TestCheckoutScriptCheckoutFailureStillFails(t *testing.T) {
	r := runCheckout(t, twoClones, true)
	if r.err == nil {
		t.Fatalf("the script succeeded though the checkout failed:\n%s", r.out)
	}
	if r.has("clone") {
		t.Errorf("a target was cloned after a failed checkout: %v", r.log)
	}
}

func TestCheckoutScriptClonesTargets(t *testing.T) {
	r := runCheckout(t, twoClones, false)
	if r.err != nil {
		t.Fatalf("script: %v\n%s", r.err, r.out)
	}
	for _, d := range []string{"linked-1/.git/config", "linked-2/.git/config"} {
		if !r.exists(d) {
			t.Errorf("%s is missing", d)
		}
	}
	for _, d := range []string{".linked-1.tmp", ".linked-2.tmp"} {
		if r.exists(d) {
			t.Errorf("temporary clone %s was left", d)
		}
	}
	// The clone's origin holds the plain URL.
	b, err := os.ReadFile(filepath.Join(r.work, "linked-1/.git/config"))
	if err != nil || !strings.Contains(string(b), "url = https://github.com/acme/api.git") {
		t.Errorf("origin config = %q, %v", b, err)
	}
}

func TestCheckoutScriptTargetCloneFailureContinues(t *testing.T) {
	for _, bad := range []string{
		"https://github.com/acme/broken.git", // git fails, leaving a partial directory
		"https://github.com/acme/slow.git",   // the time limit
		"https://github.com/acme/huge.git",   // over the size limit
	} {
		t.Run(bad, func(t *testing.T) {
			r := runCheckout(t, "linked-1 "+bad+"\nlinked-2 https://github.com/acme/web.git", false)
			if r.err != nil {
				t.Fatalf("a failed target clone stopped the script: %v\n%s", r.err, r.out)
			}
			if r.exists("linked-1") || r.exists(".linked-1.tmp") {
				t.Error("a failed clone looks complete or was left behind")
			}
			if !r.exists("linked-2/.git/config") {
				t.Error("the next target was not cloned")
			}
			if !r.exists("repo") {
				t.Error("the checkout is gone")
			}
		})
	}
}

// The limit is inclusive: a clone of exactly 2 GiB stays, one KiB more goes
// (the huge case above). The fake du reports the sizes, so the script's own
// comparison is what is tested.
func TestCheckoutScriptSizeLimitIsInclusive(t *testing.T) {
	if LinkedCloneMaxKiB != 2*1024*1024 {
		t.Fatalf("LinkedCloneMaxKiB = %d; the fake du reports exactly 2097152", LinkedCloneMaxKiB)
	}
	r := runCheckout(t, "linked-1 https://github.com/acme/exact.git", false)
	if r.err != nil {
		t.Fatalf("script: %v\n%s", r.err, r.out)
	}
	if !r.exists("linked-1/.git/config") {
		t.Error("a clone of exactly the limit was removed")
	}
	if r := runCheckout(t, "linked-1 https://github.com/acme/huge.git", false); r.exists("linked-1") {
		t.Error("a clone one KiB over the limit stayed")
	}
}

func TestCheckoutScriptHardensTargetClones(t *testing.T) {
	r := runCheckout(t, twoClones, false)
	if r.err != nil {
		t.Fatalf("script: %v\n%s", r.err, r.out)
	}
	if !r.has("timeout 120 git clone") {
		t.Errorf("a clone has no time limit: %v", r.log)
	}
	for _, want := range []string{"--depth=1", "--no-tags", "--single-branch", "core.symlinks=false", "core.hooksPath=/dev/null", "-- https://github.com/acme/api.git"} {
		if !r.has("git clone", want) {
			t.Errorf("no clone with %q: %v", want, r.log)
		}
	}
	if !r.has("clone-env allow=https prompt=0 nosystem=1 global=/dev/null header=set") {
		t.Errorf("the clone's environment is not hardened: %v", r.log)
	}
	// The token reaches git only through the environment header: not in an
	// argument, not in a URL, not in a file a clone leaves for the model.
	for _, l := range r.log {
		if strings.Contains(l, secretToken) || strings.Contains(l, "@") {
			t.Errorf("credential in a git call: %q", l)
		}
	}
	filepath.WalkDir(r.work, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(path); strings.Contains(string(b), secretToken) {
				t.Errorf("%s holds the token", path)
			}
		}
		return nil
	})
	// Nothing in the clone block names the token.
	_, block, _ := strings.Cut(checkoutScript, "LINKED_CLONES")
	for _, bad := range []string{"GIT_TOKEN", "GIT_USERNAME", "extraHeader", "git config", "remote add"} {
		if strings.Contains(block, bad) {
			t.Errorf("the clone block mentions %q", bad)
		}
	}
	// A bad directory name is skipped, never used as a path.
	r = runCheckout(t, "../escape https://github.com/acme/api.git\nlinked-9 https://github.com/acme/api.git", false)
	if r.err != nil || r.has("clone") {
		t.Errorf("a directory outside linked-1..3 was cloned: %v %v", r.err, r.log)
	}
}

// Launch refuses a clone with a credential in its URL before it creates
// anything: the URL is written to the clone's .git/config, which the model
// can read.
func TestLaunchRefusesACredentialInACloneURL(t *testing.T) {
	l, cs := fakeLauncher()
	p := linkedParams()
	p.LinkedClones[0].CloneURL = "https://x-access-token:ghs_abc@github.com/acme/api.git"
	if err := l.Launch(context.Background(), p, Secrets{GitToken: "g", LLMKey: "l", ResultToken: "r"}); err == nil {
		t.Fatal("Launch accepted a clone URL with a credential")
	} else if strings.Contains(err.Error(), "ghs_abc") {
		t.Errorf("the error repeats the credential: %v", err)
	}
	if len(cs.Actions()) != 0 {
		t.Errorf("Launch touched the cluster: %v", cs.Actions())
	}
}

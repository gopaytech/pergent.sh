package kube_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/reviewapi"
)

// This file is an external test package: reviewapi will import kube, so no
// file of package kube may import reviewapi.

func str(s string) *string { return &s }

// runAPICheckout runs the script the Job's checkout container runs, under sh
// in a temp dir, with git's configuration isolated from the machine's.
func runAPICheckout(t *testing.T, im reviewapi.Import, baseSHA string) (repo, stderr string, err error) {
	t.Helper()
	if _, lerr := exec.LookPath("git"); lerr != nil {
		t.Skip("git is not on PATH")
	}
	job := kube.BuildJob(kube.JobParams{
		Name: "pergent-review-test", Namespace: "reviews", RunID: "r", Image: "img",
		BaseSHA: im.Base, HeadSHA: im.Head, Import: im.Stream,
	})
	cmd := job.Spec.Template.Spec.InitContainers[0].Command
	if len(cmd) != 3 || cmd[0] != "sh" {
		t.Fatalf("checkout command = %q", cmd)
	}
	dir := t.TempDir()
	repo = filepath.Join(dir, "repo")
	file := filepath.Join(dir, "import")
	if werr := os.WriteFile(file, im.Stream, 0o400); werr != nil {
		t.Fatal(werr)
	}
	c := exec.Command(cmd[0], cmd[1], cmd[2])
	c.Dir = dir
	c.Env = append(os.Environ(),
		"HOME="+dir, "XDG_CONFIG_HOME="+dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"REPO_DIR="+repo, "IMPORT_FILE="+file, "BASE_SHA="+baseSHA, "HEAD_SHA="+im.Head,
	)
	var errb bytes.Buffer
	c.Stderr = &errb
	err = c.Run()
	return repo, errb.String(), err
}

func gitIn(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "HOME="+repo, "XDG_CONFIG_HOME="+repo, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

func TestAPICheckoutScript(t *testing.T) {
	im, err := reviewapi.BuildImport([]reviewapi.File{
		{Path: ".gitattributes", New: str("* text eol=crlf\n")},
		{Path: "values.yaml", Old: str("a: 1\nb: 2\n"), New: str("a: 1\nb: 3\n")},
		{Path: "win.txt", Old: str("a\r\nb\r\n"), New: str("a\r\nc\r\n")},
		{Path: "gone.txt", Old: str("bye\n")},
		{Path: "dir/new.txt", New: str("new\n")},
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("builds both commits and checks out the head", func(t *testing.T) {
		repo, stderr, err := runAPICheckout(t, im, im.Base)
		if err != nil {
			t.Fatalf("the script failed: %v\n%s", err, stderr)
		}
		if got := gitIn(t, repo, "rev-parse", "HEAD"); got != im.Head {
			t.Errorf("HEAD = %s; want %s", got, im.Head)
		}
		if got := gitIn(t, repo, "rev-parse", "refs/heads/base"); got != im.Base {
			t.Errorf("base = %s; want %s", got, im.Base)
		}
		// Detached, and the work tree holds the head's files.
		if got := gitIn(t, repo, "branch", "--show-current"); got != "" {
			t.Errorf("HEAD is on branch %s; want it detached", got)
		}
		if _, err := os.Stat(filepath.Join(repo, "gone.txt")); !os.IsNotExist(err) {
			t.Errorf("gone.txt is in the work tree: %v", err)
		}
		if got := gitIn(t, repo, "config", "core.hooksPath"); got != "/dev/null" {
			t.Errorf("core.hooksPath = %q", got)
		}
		// `* text eol=crlf` applies on checkout: the blob has LF line ends and
		// the file has CRLF, so the checkout step read the head's attributes.
		// A file that has CRLF in the blob stays as it is.
		for file, want := range map[string]string{"values.yaml": "a: 1\r\nb: 3\r\n", "win.txt": "a\r\nc\r\n"} {
			b, err := os.ReadFile(filepath.Join(repo, file))
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != want {
				t.Errorf("%s = %q; want %q", file, b, want)
			}
		}
	})

	t.Run("a wrong base is refused", func(t *testing.T) {
		_, stderr, err := runAPICheckout(t, im, strings.Repeat("0", 40))
		var exit *exec.ExitError
		if !asExit(err, &exit) || exit.ExitCode() != 1 {
			t.Fatalf("err = %v; want exit status 1", err)
		}
		if !strings.Contains(stderr, "the built base commit is not "+strings.Repeat("0", 40)) {
			t.Errorf("stderr = %q", stderr)
		}
	})
}

func asExit(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

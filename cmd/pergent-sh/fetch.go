package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/skillfetch"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// fetch-skills runs in the "send" container of a fetch Job (kube.BuildFetchJob),
// after the "clone" container cloned a public URL into --dir and wrote the
// commit to --commit-file, or its failure to --error-file. It reads the
// skills of --path in the clone, which is untrusted: no symlink is followed,
// and one where a skill would be is reported as a problem. It posts them to
// --result-url with the fetch token (PERGENT_SH_FETCH_TOKEN) as bearer. It
// holds no other secret, so it checks nothing for secrets: pergent.sh does,
// when the result arrives.

// fetchOptions are the flags of fetch-skills.
type fetchOptions struct {
	dir, path, commitFile, errorFile, resultURL string
}

const (
	msgSymlink      = "the skill is a symbolic link; it was not read"
	msgCloneUnknown = "the clone did not finish; see the log of the Job's clone container"
	msgCloneRead    = "couldn't read the skills in the clone; see the log of the Job's send container"

	// maxErrorFile and maxCommitFile bound what is read of the clone's
	// error and commit files.
	maxErrorFile, maxCommitFile = 4096, 256

	postAttempts   = 3
	postRetryDelay = 2 * time.Second
	postTimeout    = 30 * time.Second
)

// parseFetchArgs reads the flags of "fetch-skills". --path is a folder of
// the clone; --result-url an http or https URL.
func parseFetchArgs(args []string, stderr io.Writer) (fetchOptions, error) {
	flags := flag.NewFlagSet("fetch-skills", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var o fetchOptions
	flags.StringVar(&o.dir, "dir", "", "the clone")
	flags.StringVar(&o.path, "path", "", "the folder of the skills in the clone (empty: the top)")
	flags.StringVar(&o.commitFile, "commit-file", "", "the file the clone wrote its commit to")
	flags.StringVar(&o.errorFile, "error-file", "", "the file the clone wrote its failure to")
	flags.StringVar(&o.resultURL, "result-url", "", "where to post the skills")
	if err := flags.Parse(args); err != nil {
		return fetchOptions{}, err
	}
	switch {
	case flags.NArg() > 0:
		return fetchOptions{}, fmt.Errorf("unexpected argument %q\n%s", flags.Arg(0), usage)
	case o.dir == "" || o.commitFile == "" || o.errorFile == "" || o.resultURL == "":
		return fetchOptions{}, errors.New("fetch-skills needs --dir, --commit-file, --error-file and --result-url\n" + usage)
	}
	if u, err := url.Parse(o.resultURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fetchOptions{}, errors.New("--result-url must be an http or https URL")
	}
	if o.path != "" {
		for _, seg := range strings.Split(o.path, "/") {
			if seg == "" || seg == "." || seg == ".." || strings.Contains(seg, "\\") {
				return fetchOptions{}, errors.New("--path must be a folder of the clone, like skills")
			}
		}
	}
	return o, nil
}

// runFetchSkills is the fetch-skills command: it reads the token from the
// environment and posts the result, with a client that never follows a
// redirect (the token goes only to the result URL).
func runFetchSkills(ctx context.Context, o fetchOptions, stderr io.Writer) int {
	token := os.Getenv("PERGENT_SH_FETCH_TOKEN")
	if token == "" {
		fmt.Fprintln(stderr, "environment variable PERGENT_SH_FETCH_TOKEN is not set or empty")
		return 1
	}
	client := &http.Client{
		Timeout:       postTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if err := fetchSkills(ctx, o, token, client, stderr); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// fetchSkills reads the clone and posts what it read. A result whose body
// would be over skillfetch.MaxFetchBytes is posted as an error instead. Only
// a result pergent.sh did not take is an error.
func fetchSkills(ctx context.Context, o fetchOptions, token string, client *http.Client, stderr io.Writer) error {
	res := readClone(ctx, o, stderr)
	body, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("encode the result: %w", err)
	}
	if len(body) > skillfetch.MaxFetchBytes {
		fmt.Fprintf(stderr, "the result is %d bytes, over %d: sending an error instead\n", len(body), skillfetch.MaxFetchBytes)
		if body, err = json.Marshal(skillfetch.JobResult{Error: skillfetch.MsgTooLarge}); err != nil {
			return fmt.Errorf("encode the result: %w", err)
		}
	}
	if err := postResult(ctx, client, o.resultURL, token, body, stderr); err != nil {
		return err
	}
	if res.Error != "" {
		fmt.Fprintln(stderr, "sent the error:", res.Error)
	} else {
		fmt.Fprintf(stderr, "sent %d skills at %s\n", len(res.Files), res.Commit)
	}
	return nil
}

// readClone is the result to post: the clone's error, or the commit and the
// skills of the folder.
func readClone(ctx context.Context, o fetchOptions, stderr io.Writer) skillfetch.JobResult {
	text, ok, err := readSmall(o.errorFile, maxErrorFile)
	if err != nil {
		fmt.Fprintln(stderr, "reading the error file:", err)
		return skillfetch.JobResult{Error: msgCloneUnknown}
	}
	if ok {
		if text = strings.TrimSpace(text); text == "" {
			text = msgCloneUnknown
		}
		return skillfetch.JobResult{Error: text}
	}
	commit, ok, err := readSmall(o.commitFile, maxCommitFile)
	if err != nil || !ok {
		if err != nil {
			fmt.Fprintln(stderr, "reading the commit file:", err)
		}
		return skillfetch.JobResult{Error: msgCloneUnknown}
	}
	files, refusal, err := scanClone(ctx, o.dir, o.path)
	switch {
	case err != nil:
		fmt.Fprintln(stderr, "reading the skills:", err)
		return skillfetch.JobResult{Error: msgCloneRead}
	case refusal != "":
		return skillfetch.JobResult{Error: refusal}
	}
	return skillfetch.JobResult{Commit: strings.TrimSpace(commit), Files: files}
}

// readSmall reads at most max bytes of the file p; ok is false when it does
// not exist.
func readSmall(p string, max int64) (string, bool, error) {
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max))
	if err != nil {
		return "", false, err
	}
	return string(b), true, nil
}

// scanClone reads the skills of folder in the clone at dir (inputs.ScanFolder
// over inputs.DirSource, which never follows a symlink) and adds a problem
// for each symlink where a skill would be: an entry of the folder, or the
// SKILL.md of a skill folder. A text over the size of one skill is not sent.
// A folder that is, or lies below, a symlink is refused with the reason; a
// missing one has no skills.
func scanClone(ctx context.Context, dir, folder string) ([]store.FetchedFile, string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, "", err
	}
	defer root.Close()

	list := "."
	if folder != "" {
		list = folder
		segs := strings.Split(folder, "/")
		for i := range segs {
			info, err := root.Lstat(strings.Join(segs[:i+1], "/"))
			switch {
			case errors.Is(err, fs.ErrNotExist):
				return []store.FetchedFile{}, "", nil
			case err != nil:
				return nil, "", err
			case info.Mode()&fs.ModeSymlink != 0:
				return nil, fmt.Sprintf("the folder %q is a symbolic link; it was not read", folder), nil
			case !info.IsDir():
				return []store.FetchedFile{}, "", nil
			}
		}
	}

	found, err := inputs.ScanFolder(ctx, inputs.DirSource{Root: dir}, 0, folder, "")
	if err != nil {
		return nil, "", err
	}
	byPath := map[string]int{}
	for i, s := range found {
		byPath[s.Path] = i
	}
	entries, err := fs.ReadDir(root.FS(), list)
	if err != nil {
		return nil, "", err
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		switch {
		case e.Type()&fs.ModeSymlink != 0:
			// A file or a folder: never followed to find out which.
			found = append(found, inputs.Found{Name: strings.TrimSuffix(name, ".md"), Path: path.Join(folder, name), Problem: msgSymlink})
		case e.IsDir():
			skill := path.Join(folder, name, "SKILL.md")
			info, err := root.Lstat(path.Join(list, name, "SKILL.md"))
			if err != nil || info.Mode()&fs.ModeSymlink == 0 {
				continue
			}
			if i, ok := byPath[skill]; ok {
				found[i] = inputs.Found{Name: name, Path: skill, Problem: msgSymlink}
			}
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].Path < found[j].Path })

	files := make([]store.FetchedFile, 0, len(found))
	for _, s := range found {
		file := store.FetchedFile{Name: s.Name, Path: s.Path, Problem: s.Problem}
		if len(s.Content) <= inputs.MaxSkillBytes {
			file.Content = string(s.Content)
		}
		files = append(files, file)
	}
	return files, "", nil
}

// postResult posts body to resultURL with the token as bearer. A failure to
// reach pergent.sh or a 5xx is tried again, up to postAttempts times; any
// other answer but 2xx is final.
func postResult(ctx context.Context, client *http.Client, resultURL, token string, body []byte, stderr io.Writer) error {
	var last error
	for attempt := 1; attempt <= postAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(postRetryDelay):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, resultURL, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("build the request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			last = fmt.Errorf("post the result: %w", err)
			fmt.Fprintln(stderr, last)
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return nil
		case resp.StatusCode >= 500:
			last = fmt.Errorf("pergent.sh answered %s", resp.Status)
			fmt.Fprintln(stderr, last)
		default:
			return fmt.Errorf("pergent.sh refused the result: %s", resp.Status)
		}
	}
	return last
}

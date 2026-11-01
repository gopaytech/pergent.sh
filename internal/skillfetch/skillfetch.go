// Package skillfetch reads the skills of a git source for an admin to
// install into the library or to update installed ones from (D13-D19). A
// repository on a connected GitLab or GitHub instance is read by the pod,
// through the platform's API, with the review bot or the review App
// (forge.SourceReader): the admin enters no secret and no Job runs. A public
// URL is read in a Job with no token (job.go); the pod never fetches an
// address an admin typed.
//
// A read is recorded as a skill fetch (store.SkillFetch): the skills found,
// each with its text or the problem that keeps it from being installed, and
// the commit read. It is kept FetchKeep for the admin to choose from. Nothing
// here changes the library.
package skillfetch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// Limits of one fetch: the skills found (the library holds no more), the text
// kept, how long a read may take, and how long its result is kept.
const MaxFetchFiles, MaxFetchBytes, FetchTimeout, FetchKeep = 200, 4 << 20, 2 * time.Minute, time.Hour

// maxRunning is how many reads run at once; others wait their turn, within
// their FetchTimeout.
const maxRunning = 2

// completeTimeout bounds recording a read's result.
const completeTimeout = 10 * time.Second

// Messages a fetch records, for the admin. They never hold a platform's
// answer or a secret.
const (
	msgSecret       = "the skill contains a secret pergent.sh holds"
	msgNoText       = "the skill has no text"
	msgSecretsCheck = "couldn't check the skills for secrets pergent.sh holds; try again"
	msgReadFailed   = "couldn't read the repository; try again, or see the pergent.sh log"
)

var (
	msgTooMany = fmt.Sprintf("the folder has more than %d skills; choose a smaller folder", MaxFetchFiles)
	// MsgTooLarge is also what a fetch Job posts for a folder of too much text.
	MsgTooLarge = fmt.Sprintf("the skills come to more than %d MiB; choose a smaller folder", MaxFetchBytes>>20)
	msgTooSlow  = fmt.Sprintf("reading the repository took longer than %d minutes", int(FetchTimeout/time.Minute))
)

// Request is what an admin asks to read: a repository (Repo) of a connected
// instance, or a public git URL; the branch or tag Ref and the folder Path
// in it (empty: the top). Purpose is store.FetchInstall or store.FetchUpdate
// (empty: install); an update compares with the skills installed at FromRef
// (empty: Ref). Actor and ActorID are who asked.
type Request struct {
	Instance, Repo, URL, Ref, FromRef, Path, Purpose, Actor string
	ActorID                                                 int64
}

// RequestError is a request that can't be read as given; its text is for the
// admin.
type RequestError string

func (e RequestError) Error() string { return string(e) }

// ErrNotConnected is a request for an instance that has no reader: it is
// unknown, or not connected now.
var ErrNotConnected = errors.New("the instance is not connected")

var (
	errTooMany = errors.New("too many skills")
	errSecrets = errors.New("the held secrets are unknown")
)

// Fetcher starts reads of skill sources and sweeps the old ones.
type Fetcher struct {
	Store *store.Store
	// Readers returns the reader of an instance's repositories: its review
	// bot (GitLab) or review App (GitHub).
	Readers func(instance string) (forge.SourceReader, bool)
	// Secrets lists every secret pergent.sh holds
	// (instances.Registry.HeldSecrets); an error fails the read.
	Secrets      func(ctx context.Context) ([]string, error)
	CallbackBase string // config callback_url
	Now          func() time.Time
	Log          *slog.Logger

	// A public URL is read by a Job: Launcher starts it in Namespace (the
	// Jobs namespace), with GitImage (jobs.image) for the clone and Image
	// (PERGENT_SH_IMAGE, pergent.sh's own) to send the skills back.
	Launcher        Launcher
	Namespace       string
	GitImage, Image string

	once  sync.Once
	slots chan struct{}
	wg    sync.WaitGroup
}

func (f *Fetcher) now() time.Time {
	if f.Now == nil {
		return time.Now()
	}
	return f.Now()
}

func (f *Fetcher) log() *slog.Logger {
	if f.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return f.Log
}

// ValidateURL accepts a public https git URL: a host, and no user name,
// password, query or fragment. Its error is a RequestError.
func ValidateURL(raw string) error {
	if raw == "" || len(raw) > 2048 || strings.ContainsFunc(raw, isSpaceOrControl) {
		return RequestError("give the https URL of a git repository")
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Opaque != "":
		return RequestError("give the https URL of a git repository")
	case u.Scheme != "https":
		return RequestError("only https URLs can be installed from")
	case u.User != nil:
		return RequestError("the URL must not hold a user name or password: only public repositories can be installed from a URL")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#"):
		return RequestError("the URL must not have a query or a fragment")
	case u.Hostname() == "":
		return RequestError("the URL needs a host")
	}
	return nil
}

// Start records a read of r, running, and returns its ID: a repository of a
// connected instance is read in the background, at most maxRunning at once
// and within FetchTimeout; a public URL by a Job (startJob). A request that can't be read as given is a
// RequestError; an instance with no reader is ErrNotConnected.
func (f *Fetcher) Start(ctx context.Context, r Request) (string, error) {
	r, err := Normalize(r)
	if err != nil {
		return "", err
	}
	if r.URL != "" {
		return f.startJob(ctx, r)
	}
	var reader forge.SourceReader
	ok := false
	if f.Readers != nil {
		reader, ok = f.Readers(r.Instance)
	}
	if !ok || reader == nil {
		return "", ErrNotConnected
	}
	now := f.now()
	id, err := f.Store.CreateSkillFetch(ctx, store.SkillFetch{Kind: store.SourceKindInstance, Instance: r.Instance,
		Repo: r.Repo, Ref: r.Ref, FromRef: r.FromRef, Path: r.Path, Purpose: r.Purpose,
		RequestedBy: r.Actor, RequestedByID: r.ActorID, CreatedAt: now, ExpiresAt: now.Add(FetchKeep)}, nil)
	if err != nil {
		return "", err
	}
	f.once.Do(func() { f.slots = make(chan struct{}, maxRunning) })
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), FetchTimeout)
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		defer cancel()
		f.run(rctx, id, reader, r)
	}()
	return id, nil
}

// wait waits for the reads started to end.
func (f *Fetcher) wait() { f.wg.Wait() }

// run reads r once a slot is free and records the result.
func (f *Fetcher) run(ctx context.Context, id string, reader forge.SourceReader, r Request) {
	var (
		commit string
		files  []store.FetchedFile
		err    error
	)
	select {
	case f.slots <- struct{}{}:
		commit, files, err = f.read(ctx, reader, r)
		<-f.slots
	case <-ctx.Done():
		err = ctx.Err()
	}
	errText := ""
	if err != nil {
		errText = f.errorText(ctx, id, err)
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), completeTimeout)
	defer cancel()
	ok, err := f.Store.CompleteSkillFetch(cctx, id, nil, commit, errText, files, f.now())
	switch {
	case err != nil:
		f.log().Error("skill fetch: recording the result failed", "fetch", id, "error", err.Error())
	case !ok:
		f.log().Warn("skill fetch: the result came after the fetch ended", "fetch", id)
	default:
		f.log().Info("skill fetch: read", "fetch", id, "instance", r.Instance, "ref", r.Ref,
			"commit", commit, "skills", len(files), "failed", errText != "")
	}
}

// errorText is the message recorded for a failed read. One the admin can't
// act on is logged.
func (f *Fetcher) errorText(ctx context.Context, id string, err error) string {
	switch {
	case ctx.Err() != nil:
		return msgTooSlow
	case errors.Is(err, forge.ErrSourceNotFound):
		return forge.ErrSourceNotFound.Error()
	case errors.Is(err, errTooMany):
		return msgTooMany
	case errors.Is(err, errSecrets):
		f.log().Error("skill fetch: reading the held secrets failed", "fetch", id, "error", err.Error())
		return msgSecretsCheck
	case errors.As(err, new(sizeError)):
		return MsgTooLarge
	}
	f.log().Warn("skill fetch: reading the repository failed", "fetch", id, "error", err.Error())
	return msgReadFailed
}

// read resolves r.Ref to a commit and reads the skills of r.Path there.
func (f *Fetcher) read(ctx context.Context, reader forge.SourceReader, r Request) (string, []store.FetchedFile, error) {
	// GitLab can't tell a commit that doesn't exist from a missing file, so
	// the ref is resolved first and the files are read at that commit.
	commit, err := reader.ResolveRef(ctx, r.Repo, r.Ref)
	if err != nil {
		return "", nil, err
	}
	found, err := inputs.ScanFolder(ctx, source{reader: reader, project: r.Repo}, 0, r.Path, commit)
	if err != nil {
		return "", nil, err
	}
	secrets, err := f.heldSecrets(ctx)
	if err != nil {
		return "", nil, err
	}
	files, err := checkFiles(found, secrets)
	return commit, files, err
}

// sizeError is a fetch whose text is over MaxFetchBytes.
type sizeError struct{ total int }

func (e sizeError) Error() string { return fmt.Sprintf("the skills come to %d bytes", e.total) }

// checkFiles turns the skills a scan found into a fetch's files. A skill
// with no text or holding a held secret gets a problem; the text of one over
// MaxSkillBytes or holding a secret is not kept. More than MaxFetchFiles
// skills, or more than MaxFetchBytes of text kept, is an error.
func checkFiles(found []inputs.Found, secrets []string) ([]store.FetchedFile, error) {
	if len(found) > MaxFetchFiles {
		return nil, errTooMany
	}
	files := make([]store.FetchedFile, 0, len(found))
	total := 0
	for _, s := range found {
		file := store.FetchedFile{Name: s.Name, Path: s.Path, Problem: s.Problem}
		switch {
		case inputs.ContainsSecret(s.Content, secrets):
			file.Problem = msgSecret
		case len(s.Content) > inputs.MaxSkillBytes:
			// The scan's problem names the size; the text is not kept.
		default:
			file.Content = string(s.Content)
			if file.Problem == "" && strings.TrimSpace(file.Content) == "" {
				file.Problem = msgNoText
			}
		}
		total += len(file.Content)
		if total > MaxFetchBytes {
			return nil, sizeError{total: total}
		}
		files = append(files, file)
	}
	return files, nil
}

// Sweep fails the reads still running past FetchTimeout and a minute (their
// reader went away) and deletes those past their expiry.
func (f *Fetcher) Sweep(ctx context.Context) error {
	now := f.now()
	failed, err := f.Store.FailStaleSkillFetches(ctx, now.Add(-FetchTimeout-time.Minute))
	if err != nil {
		return fmt.Errorf("failing stale skill fetches: %w", err)
	}
	pruned, err := f.Store.PruneSkillFetches(ctx, now)
	if err != nil {
		return fmt.Errorf("pruning skill fetches: %w", err)
	}
	if failed > 0 || pruned > 0 {
		f.log().Info("skill fetch: swept", "failed", failed, "pruned", pruned)
	}
	return nil
}

// source is an inputs.Source over a repository read by its path: the
// project ID ScanFolder passes is ignored, and refs are commits.
type source struct {
	reader  forge.SourceReader
	project string
}

var _ inputs.Source = source{}

// GetFile reads a file; one over what the platform reader reads is
// inputs.ErrFileTooLarge, which ScanFolder reports as the skill's problem.
func (s source) GetFile(ctx context.Context, _ int64, p, commit string) ([]byte, bool, error) {
	b, ok, err := s.reader.GetFileAt(ctx, s.project, p, commit)
	if errors.Is(err, forge.ErrFileTooLarge) {
		return nil, false, inputs.ErrFileTooLarge
	}
	return b, ok, err
}

// ListTree lists a folder. One with more entries ScanFolder would read than
// MaxFetchFiles is refused before any is read.
func (s source) ListTree(ctx context.Context, _ int64, dir, commit string) ([]forge.TreeEntry, error) {
	entries, err := s.reader.ListTreeAt(ctx, s.project, dir, commit)
	if err != nil {
		return nil, err
	}
	n := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name, ".") && (e.Type == "tree" || (e.Type == "blob" && path.Ext(e.Name) == ".md")) {
			n++
		}
	}
	if n > MaxFetchFiles {
		return nil, errTooMany
	}
	return entries, nil
}

// Normalize checks r and returns it as Start reads it: the purpose (empty is
// install), a repository or a URL, the ref (and the from-ref of an update,
// empty: the ref) and the folder, without slashes at their ends. Its error
// is a RequestError. A caller that checks who may read the source checks
// the normalized one.
func Normalize(r Request) (Request, error) {
	r.Instance, r.URL = strings.TrimSpace(r.Instance), strings.TrimSpace(r.URL)
	r.Repo = strings.Trim(strings.TrimSpace(r.Repo), "/")
	r.Ref, r.FromRef = strings.TrimSpace(r.Ref), strings.TrimSpace(r.FromRef)
	switch r.Purpose {
	case "":
		r.Purpose = store.FetchInstall
	case store.FetchInstall, store.FetchUpdate:
	default:
		return r, RequestError(`the purpose must be "install" or "update"`)
	}
	switch {
	case r.URL != "" && (r.Instance != "" || r.Repo != ""):
		return r, RequestError("give a repository of an instance or a URL, not both")
	case r.URL != "":
		if err := ValidateURL(r.URL); err != nil {
			return r, err
		}
	case r.Instance == "" || r.Repo == "":
		return r, RequestError("give an instance and a repository, or a URL")
	case len(r.Repo) > 255 || strings.ContainsFunc(r.Repo, isSpaceOrControl) || strings.Contains(r.Repo, ".."):
		return r, RequestError("the repository is not a valid path, like group/project")
	}
	if !validRef(r.Ref) {
		return r, RequestError("give a valid branch or tag")
	}
	switch {
	case r.Purpose == store.FetchInstall:
		r.FromRef = ""
	case r.FromRef == "":
		r.FromRef = r.Ref
	case !validRef(r.FromRef):
		return r, RequestError("the branch or tag the skills came from is not valid")
	}
	p, ok := cleanFolder(r.Path)
	if !ok {
		return r, RequestError("the folder must be a path in the repository, like skills")
	}
	r.Path = p
	return r, nil
}

// validRef follows git's rules for a ref name, enough to keep it a name: no
// space, control character or ~^:?*[\, no "..", "@{" or "//", not starting
// with "-" or "/" and not ending with "/", "." or ".lock".
func validRef(ref string) bool {
	return ref != "" && len(ref) <= 255 &&
		!strings.ContainsFunc(ref, isSpaceOrControl) && !strings.ContainsAny(ref, "~^:?*[\\") &&
		!strings.Contains(ref, "..") && !strings.Contains(ref, "@{") && !strings.Contains(ref, "//") &&
		!strings.HasPrefix(ref, "-") && !strings.HasPrefix(ref, "/") &&
		!strings.HasSuffix(ref, "/") && !strings.HasSuffix(ref, ".") && !strings.HasSuffix(ref, ".lock")
}

// cleanFolder is folder without slashes at its ends; ok is false when it is
// not a plain path inside a repository.
func cleanFolder(folder string) (string, bool) {
	folder = strings.Trim(strings.TrimSpace(folder), "/")
	if folder == "" {
		return "", true
	}
	if len(folder) > 1024 || strings.ContainsFunc(folder, isControl) || strings.Contains(folder, "\\") {
		return "", false
	}
	for _, seg := range strings.Split(folder, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	return folder, true
}

func isControl(r rune) bool        { return r < 0x20 || r == 0x7f }
func isSpaceOrControl(r rune) bool { return r == ' ' || isControl(r) }

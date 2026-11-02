package skillfetch

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// A public URL is read by a short Kubernetes Job, never by the pod (D18):
// the Job clones the ref with no token, under the review-Job NetworkPolicy,
// and posts what it read to /skill-fetches/{id}/result with a one-time token,
// its only secret. The pod keeps only the token's hash; the first result
// with the token spends it. What the Job posts is checked here as a read by
// the pod is: held secrets and oversize text are not kept.

// Launcher starts a fetch Job (kube.Launcher).
type Launcher interface {
	LaunchFetch(ctx context.Context, p kube.FetchJobParams, token string) error
}

const (
	msgNoImage     = "installing from a URL needs PERGENT_SH_IMAGE (set by the chart)"
	msgJobStart    = "couldn't start the Job that reads the URL; see the pergent.sh log"
	msgJobUnusable = "the Job that read the URL sent a result pergent.sh couldn't use"
)

// maxJobText bounds an error or problem a Job reports, in bytes.
const maxJobText = 500

// JobResult is what a fetch Job posts: the commit it read and the skills of
// the folder, or Error when it could not read them.
type JobResult struct {
	Commit string              `json:"commit"`
	Error  string              `json:"error"`
	Files  []store.FetchedFile `json:"files"`
}

// JobOutcome is what became of a result a fetch Job posted.
type JobOutcome int

const (
	// JobStored: the result was recorded and the token spent.
	JobStored JobOutcome = iota + 1
	// JobUnauthorized: no fetch Job of that ID holds that token.
	JobUnauthorized
	// JobConflict: the fetch has ended (a result came, or it was swept).
	JobConflict
)

// commitPattern is a full SHA-1 or SHA-256 commit ID.
var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// startJob records a read of the URL of r, with the hash of a fresh token,
// and starts the Job that reads it. A Job that can't be started fails the
// fetch at once; its ID is still returned.
func (f *Fetcher) startJob(ctx context.Context, r Request) (string, error) {
	if f.Image == "" || f.Launcher == nil {
		return "", RequestError(msgNoImage)
	}
	token, hash, err := mintToken()
	if err != nil {
		return "", err
	}
	now := f.now()
	id, err := f.Store.CreateSkillFetch(ctx, store.SkillFetch{Kind: store.SourceKindURL, URL: r.URL,
		Ref: r.Ref, FromRef: r.FromRef, Path: r.Path, Purpose: r.Purpose,
		RequestedBy: r.Actor, RequestedByID: r.ActorID, CreatedAt: now, ExpiresAt: now.Add(FetchKeep)}, hash)
	if err != nil {
		return "", err
	}
	p := kube.FetchJobParams{
		Name: kube.FetchJobName(id), Namespace: f.Namespace, FetchID: id,
		GitImage: f.GitImage, Image: f.Image,
		URL: r.URL, Ref: r.Ref, Path: r.Path,
		ResultURL: strings.TrimRight(f.CallbackBase, "/") + "/skill-fetches/" + id + "/result",
		Timeout:   FetchTimeout,
	}
	if err := f.Launcher.LaunchFetch(ctx, p, token); err != nil {
		f.log().Error("skill fetch: starting the Job failed", "fetch", id, "job", p.Name, "error", err.Error())
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), completeTimeout)
		defer cancel()
		if _, err := f.Store.CompleteSkillFetch(cctx, id, hash, "", msgJobStart, nil, f.now()); err != nil {
			f.log().Error("skill fetch: recording the result failed", "fetch", id, "error", err.Error())
		}
		return id, nil
	}
	f.log().Info("skill fetch: Job started", "fetch", id, "job", p.Name, "url", r.URL, "ref", r.Ref)
	return id, nil
}

// CompleteJob records the result body a fetch Job posted with token: the
// files after the checks a read by the pod gets, or the Job's error. A body
// pergent.sh can't use fails the fetch. Nothing changes unless token is the
// fetch's; a fetch that has ended is JobConflict.
func (f *Fetcher) CompleteJob(ctx context.Context, id, token string, body []byte) (JobOutcome, error) {
	fetch, err := f.Store.SkillFetch(ctx, id)
	switch {
	case errors.Is(err, store.ErrSkillFetchNotFound):
		return JobUnauthorized, nil
	case err != nil:
		return 0, err
	case fetch.Kind != store.SourceKindURL:
		return JobUnauthorized, nil
	case fetch.Status != store.FetchRunning:
		return JobConflict, nil
	}
	// The route is public: a wrong token costs one cheap query, not the read
	// of the held secrets and the checks of a 4 MiB body. The spend below
	// stays atomic.
	hash := hashToken(token)
	match, err := f.Store.SkillFetchTokenIs(ctx, id, hash, f.now())
	switch {
	case err != nil:
		return 0, err
	case !match:
		return JobUnauthorized, nil
	}
	commit, errText, files := f.jobResult(ctx, id, fetch.Path, body)
	ok, err := f.Store.CompleteSkillFetch(ctx, id, hash, commit, errText, files, f.now())
	if err != nil {
		return 0, err
	}
	if !ok {
		// Another token, or the fetch ended since it was read.
		if again, err := f.Store.SkillFetch(ctx, id); err == nil && again.Status != store.FetchRunning {
			return JobConflict, nil
		}
		return JobUnauthorized, nil
	}
	f.log().Info("skill fetch: read by a Job", "fetch", id, "url", fetch.URL, "ref", fetch.Ref,
		"commit", commit, "skills", len(files), "failed", errText != "")
	return JobStored, nil
}

// JobTooLarge fails the fetch whose Job posted a body over MaxFetchBytes,
// when token is the fetch's.
func (f *Fetcher) JobTooLarge(ctx context.Context, id, token string) error {
	_, err := f.Store.CompleteSkillFetch(ctx, id, hashToken(token), "", MsgTooLarge, nil, f.now())
	return err
}

// jobResult reads a Job's body into what the fetch records: the commit and
// the checked files, or the error text.
func (f *Fetcher) jobResult(ctx context.Context, id, folder string, body []byte) (commit, errText string, files []store.FetchedFile) {
	var res JobResult
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&res); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		f.log().Warn("skill fetch: a Job sent a body that is not a result", "fetch", id)
		return "", msgJobUnusable, nil
	}
	secrets, err := f.heldSecrets(ctx)
	if err != nil {
		return "", f.errorText(ctx, id, err), nil
	}
	if res.Error != "" {
		text := JobText(res.Error)
		if text == "" || inputs.ContainsSecret([]byte(text), secrets) {
			text = msgReadFailed
		}
		return "", text, nil
	}
	if !commitPattern.MatchString(res.Commit) {
		f.log().Warn("skill fetch: a Job sent a commit that is not one", "fetch", id)
		return "", msgJobUnusable, nil
	}
	found := make([]inputs.Found, 0, len(res.Files))
	for _, file := range res.Files {
		if !validJobFile(file, folder) {
			f.log().Warn("skill fetch: a Job sent a skill outside its folder or with a bad name", "fetch", id)
			return "", msgJobUnusable, nil
		}
		s := inputs.Found{Name: file.Name, Path: file.Path, Problem: JobText(file.Problem), Content: []byte(file.Content)}
		if s.Problem == "" {
			// The Job checked it too; the name, size and text are checked
			// again here (the size first, so a huge text is never parsed).
			if err := inputs.CheckSkill(s.Name, s.Content); err != nil {
				s.Problem = err.Error()
			}
		}
		found = append(found, s)
	}
	files, err = checkFiles(found, secrets)
	if err != nil {
		return "", f.errorText(ctx, id, err), nil
	}
	return res.Commit, "", files
}

// heldSecrets lists the secrets pergent.sh holds; an error is errSecrets.
func (f *Fetcher) heldSecrets(ctx context.Context) ([]string, error) {
	if f.Secrets == nil {
		return nil, fmt.Errorf("%w: no secrets reader", errSecrets)
	}
	secrets, err := f.Secrets(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errSecrets, err)
	}
	return secrets, nil
}

// validJobFile reports whether a file a Job sent has a name and a plain path
// in folder: no "..", ".", empty or hidden segment, no backslash or control
// character.
func validJobFile(file store.FetchedFile, folder string) bool {
	if file.Name == "" || len(file.Name) > 255 || strings.ContainsFunc(file.Name, isControl) {
		return false
	}
	p := file.Path
	if p == "" || len(p) > 1024 || strings.ContainsFunc(p, isControl) || strings.Contains(p, "\\") {
		return false
	}
	if folder != "" && !strings.HasPrefix(p, folder+"/") {
		return false
	}
	for _, seg := range strings.Split(strings.TrimPrefix(p, folder+"/"), "/") {
		if seg == "" || seg == "." || seg == ".." || strings.HasPrefix(seg, ".") {
			return false
		}
	}
	return true
}

// JobText is s, a Job's error or problem, as one line of valid UTF-8 of at
// most maxJobText bytes: control characters become spaces, runs of spaces
// one.
func JobText(s string) string {
	s = strings.ToValidUTF8(s, "?")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxJobText {
		s = s[:maxJobText]
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

// mintToken returns a fresh fetch token (32 random bytes, base64url without
// padding) and its SHA-256, which is all the store keeps.
func mintToken() (token string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("mint fetch token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token), nil
}

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

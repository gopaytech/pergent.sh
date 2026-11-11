package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/reviewapi"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/usage"
)

// The failure reasons of an API review. They reach the caller, so they name
// no file, secret, model or other client.
const (
	reasonClientGone  = "the API client was removed"
	reasonClientModel = "the model chosen for this API client can't be used: an admin can change it in the client's settings"
	reasonInputsGone  = "the review's files are no longer stored"
	// reasonClientNoSkills is the text of the 409 a request gets for it.
	reasonClientNoSkills  = "this client has no skills: an admin chooses them in its settings"
	reasonClientTooLarge  = "the skills of this API client are larger than 900 KiB"
	reasonAPICouldntStart = "pergent.sh couldn't start the review"
	reasonAPIUnusable     = "pergent sent a result pergent.sh couldn't use"
)

// dispatchAPI starts the Job of an API review: the client's skills, on its
// model, with the dashboard's defaults for the timeout, step limit and memory,
// and the files the caller sent, which its checkout builds the two commits
// from. It calls no forge, and sets no status and no attention item. As for a
// merge request, a store or Kubernetes error is retried and anything else
// fails the review.
func (d *dispatcher) dispatchAPI(ctx context.Context) error {
	e, run := d.e, d.run
	d.api = true
	d.log = e.log().With("run", run.ID, "kind", store.KindAPI, "head", run.HeadSHA)

	if run.APIClientID == nil {
		return d.fail(ctx, reasonClientGone)
	}
	client, found, err := e.Store.APIClientByID(ctx, *run.APIClientID)
	if err != nil {
		return d.retry(ctx, fmt.Errorf("load the API client: %w", err))
	}
	if !found {
		return d.fail(ctx, reasonClientGone)
	}
	d.log = d.log.With("api_client", client.Name)

	// The skills are read now, as for a merge request: an edit made since the
	// request is what reviews. A client names its skills; an empty list is
	// never the default list.
	if len(client.Skills) == 0 {
		return d.fail(ctx, reasonClientNoSkills)
	}
	resolved, err := e.Store.ResolveLibrary(ctx, client.Skills)
	if err != nil {
		return d.retry(ctx, fmt.Errorf("read the skill library: %w", err))
	}
	if len(resolved.Missing) > 0 {
		return d.fail(ctx, fmt.Sprintf("skill %q is not in the skill library", resolved.Missing[0]))
	}
	if len(resolved.Skills) == 0 {
		return d.fail(ctx, reasonClientNoSkills)
	}

	// A client that chose a model runs on it or on nothing, as a repository
	// does.
	var model instances.Model
	if client.ModelID != "" {
		var ok bool
		if model, ok = e.registry().ModelByID(client.ModelID); !ok {
			d.log.Warn("the model chosen for this API client can't be used", "model_id", client.ModelID)
			return d.fail(ctx, reasonClientModel)
		}
	} else {
		var ok bool
		if model, ok = e.registry().DefaultModel(); !ok {
			e.warnEvery("no-llm", msgNoLLM)
			return d.postpone(ctx, e.now().Add(waitDelay))
		}
	}
	llm, llmKey := model.LLM, model.Key
	d.provider, d.model = llm.Provider, llm.Model
	d.secrets = append(d.secrets, llmKey)

	// What a repository switched on in the dashboard gets when it sets
	// nothing.
	defaults, err := e.settings().BrowserDefaults(ctx, "", "")
	if err != nil {
		return d.retry(ctx, fmt.Errorf("load the dashboard's defaults: %w", err))
	}

	review, found, err := e.Store.APIReviewByID(ctx, run.ID)
	if err != nil {
		return d.retry(ctx, fmt.Errorf("load the API review: %w", err))
	}
	if !found {
		return d.fail(ctx, reasonInputsGone)
	}
	sealedInputs, found, err := e.Store.APIReviewInputs(ctx, run.ID)
	if err != nil {
		return d.retry(ctx, fmt.Errorf("load the review's files: %w", err))
	}
	if !found {
		return d.fail(ctx, reasonInputsGone)
	}
	// Inputs that can't be opened or read are as good as gone: no retry
	// makes them readable.
	plain, err := e.Box.Open(sealedInputs)
	if err != nil {
		d.log.Warn("the review's files can't be opened", "error", d.redact(err.Error()))
		return d.fail(ctx, reasonInputsGone)
	}
	var sent reviewapi.Sealed
	if err := json.Unmarshal(plain, &sent); err != nil || sent.Import == nil {
		d.log.Warn("the review's files can't be read", "error", errText(err))
		return d.fail(ctx, reasonInputsGone)
	}

	in := &inputs.Inputs{Skills: map[string][]byte{}, Sources: map[string]string{}, Versions: map[string]int{}}
	for name, sk := range resolved.Skills {
		in.Skills[name] = []byte(sk.Content)
		in.Sources[name] = inputs.SourceLibrary
		in.Versions[name] = sk.Version
	}
	in.AddDescription(review.Title, sent.Description)
	hadDescription := len(in.Description) > 0
	if err := in.Fit(inputs.ConfigMapLimit); err != nil {
		d.log.Warn("the review's inputs don't fit", "error", d.redact(err.Error()))
		return d.fail(ctx, reasonClientTooLarge)
	}
	if hadDescription && in.Description == nil {
		d.log.Warn("reviewing without the change's title and description: the inputs are too large for them")
	}

	if err := e.Store.SetRunModel(ctx, run.ID, d.provider, d.model); err != nil {
		return d.retry(ctx, fmt.Errorf("record the review's model: %w", err))
	}
	token, tokenHash, err := mintToken()
	if err != nil {
		return d.retry(ctx, err)
	}
	d.secrets = append(d.secrets, token)

	now := e.now()
	allowance := kube.CloneAllowance(0, 0)
	expires := now.Add(kube.Deadline(defaults.JobTimeout, allowance) + tokenGrace)
	name := kube.JobName(run.ID)
	params := kube.JobParams{
		Name:           name,
		RunID:          run.ID,
		Image:          e.Config.Jobs.Image,
		Timeout:        defaults.JobTimeout,
		CloneAllowance: allowance,
		Concurrency:    e.Config.Jobs.Concurrency,
		MaxTurns:       defaults.MaxTurns,
		BaseSHA:        run.BaseSHA,
		HeadSHA:        run.HeadSHA,
		ResultURL:      strings.TrimRight(e.Config.CallbackURL, "/") + "/runs/" + run.ID + "/result",
		MaxCommentSize: kube.CommentLimitGitLab,
		LLM:            llm,
		Resources:      jobResources(e.Config.Jobs.Resources, defaults.JobMemory),
		WorkSize:       e.Config.Jobs.WorkSize,
		Inputs:         in,
		Import:         sent.Import,
		APIClient:      client.Name,
	}
	if err := e.Launcher.Launch(ctx, params, kube.Secrets{LLMKey: llmKey, ResultToken: token}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			d.deleteJob(ctx, name)
		}
		return d.retry(ctx, fmt.Errorf("launch job: %w", err))
	}

	// MarkDispatched matches no row once the run was superseded, or its client
	// removed (DeleteAPIClient locks the run first): the Job is deleted then,
	// as for a merge request.
	err = e.Store.MarkDispatched(ctx, run.ID, name, run.BaseSHA, "", tokenHash, expires, now)
	if errors.Is(err, store.ErrNotRunning) {
		d.log.Info("review was superseded or its client removed while its job started; deleting the job", "job", name)
		d.deleteJob(ctx, name)
		return nil
	}
	if err != nil {
		d.deleteJob(ctx, name)
		return d.retry(ctx, fmt.Errorf("record dispatch: %w", err))
	}
	d.log.Info("review job started", "job", name)
	// The Job has the files: the stored copy goes. One left behind is dropped
	// by the next pass (DropSpentAPIInputs).
	if err := e.Store.DropAPIInputs(ctx, run.ID); err != nil {
		d.log.Warn("couldn't drop the review's stored files; the next pass will", "error", d.redact(err.Error()))
	}
	if err := e.Store.SetRunInputs(ctx, run.ID, llm.Model, defaults.MaxTurns, in.Sources, in.Versions, skillHashes(in.Skills)); err != nil {
		d.log.Info("couldn't record the review's model, step limit and skills for the dashboard", "error", d.redact(err.Error()))
	}
	return nil
}

// errText is err's text, or "" for none.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// deleteFiles removes the Secret of an API review's files, which only its
// checkout reads. A failure is logged: the Job's deletion removes it too.
func (e *Engine) deleteFiles(ctx context.Context, run store.Run, log *slog.Logger) {
	if err := e.Launcher.DeleteFiles(ctx, run.JobName); err != nil {
		log.Warn("couldn't delete the review's files; the job's deletion will", "error", e.redact(err.Error()))
	}
}

// publishAPI finishes an API review: its result, mapped to the v1 body, is
// stored with the decision, and the run is published, in one step. Nothing is
// posted to a forge. A result that can't be used replaces the stored result
// with a failure at delivery first, so the review reads as failed everywhere,
// never as a pass. An error means the review was not finished.
func (e *Engine) publishAPI(ctx context.Context, run store.Run, log *slog.Logger) error {
	review, found, err := e.Store.APIReviewByID(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("load the API review: %w", err)
	}
	if !found {
		// Removed with its client since it was listed: nothing is left to finish.
		log.Info("not publishing the API review: it was removed with its client")
		return nil
	}

	var usages []store.SkillUsage
	hadResult := run.Result != nil
	if hadResult {
		if usages, err = usage.FromResult(run.Result, run); err != nil {
			log.Warn("couldn't read the result's usage", "error", err.Error())
			usages = nil
		}
	}
	var res *result.Result
	if run.FailureReason == "" {
		var why string
		if res, why = apiResult(run); res == nil {
			log.Warn("stored result can't be used", "error", why)
			f := store.Failure{Reason: reasonAPIUnusable, Step: kube.StepDelivery}
			if run.Result != nil {
				if err := e.Store.ReplaceWithFailure(ctx, run.ID, f); err != nil {
					return fmt.Errorf("replace the unusable result: %w", err)
				}
			}
			run.Result, run.FailureReason, run.FailureStep = nil, f.Reason, f.Step
		}
	}

	// The body is built as the run will be once published: StatusOf and
	// FailureOf read the published state.
	review.Run = run
	review.Run.State = store.Published
	verdict, skills, u := reviewapi.Map(res, usages, nil, e.redact)
	body := reviewapi.Pending(review)
	finished := e.now().UTC()
	body.FinishedAt = &finished
	body.Verdict, body.Skills = verdict, skills
	if body.Failure != nil {
		body.Verdict = reviewapi.VerdictFailed
	}
	if hadResult {
		// What the Job spent, also on a result that can't be used.
		body.Usage = &u
	}
	response, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode the API review: %w", err)
	}

	dec := store.RunDecision{Outcome: apiOutcome(body.Verdict)}
	if body.Failure != nil {
		dec.Reason = body.Failure.Reason
	}
	if err := e.Store.FinishAPIReview(ctx, run.ID, response, dec); err != nil {
		return fmt.Errorf("finish the API review: %w", err)
	}
	log.Info("API review published", "status", body.Status, "verdict", body.Verdict)
	return nil
}

// apiResult is the stored result of run when it can be used: it parses and is
// for the run's own base and head. Otherwise it is nil, and why says why (for
// the log only).
func apiResult(run store.Run) (*result.Result, string) {
	if run.Result == nil {
		return nil, "the run has no result"
	}
	r, err := result.Parse(run.Result)
	if err != nil {
		return nil, err.Error()
	}
	if r.Base != run.BaseSHA || r.Head != run.HeadSHA {
		return nil, fmt.Sprintf("it is for a different commit (base %q, head %q) than the one reviewed (base %s, head %s)",
			r.Base, r.Head, run.BaseSHA, run.HeadSHA)
	}
	return r, ""
}

// apiOutcome is the run's recorded outcome for a v1 verdict. Anything else is
// failed.
func apiOutcome(verdict string) string {
	switch verdict {
	case reviewapi.VerdictHasBlockingIssues:
		return store.OutcomeBlocking
	case reviewapi.VerdictNothingReviewed:
		return store.OutcomeNothingReviewed
	case reviewapi.VerdictNoBlockingIssues:
		return store.OutcomeNoBlocking
	}
	return store.OutcomeFailed
}

// dropSpentAPIInputs clears the sealed files no Job will need again
// (DropSpentAPIInputs): a backstop for a drop dispatch missed, and the files
// of a review superseded or failed before its Job.
func (e *Engine) dropSpentAPIInputs(ctx context.Context) error {
	n, err := e.Store.DropSpentAPIInputs(ctx)
	if err != nil {
		return fmt.Errorf("drop the files of finished API reviews: %w", err)
	}
	if n > 0 {
		e.log().Info("dropped the stored files of API reviews that no longer need them", "reviews", n)
	}
	return nil
}

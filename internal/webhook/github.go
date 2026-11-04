package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/syncmap"
)

const (
	githubEventHeader     = "X-GitHub-Event"
	githubSignatureHeader = "X-Hub-Signature-256"
	githubSignaturePrefix = "sha256="
	githubPullRequest     = "pull_request"
	githubIssueComment    = "issue_comment"
	// githubReviewComment is delivered for a comment on a line of the diff,
	// the replies in a finding's thread among them.
	githubReviewComment = "pull_request_review_comment"
)

// GitHubHandler serves POST /webhooks/github/{instance}: the review App's
// pull_request webhook. Like the GitLab handler it authenticates the event,
// queues work and answers fast; the engine does the rest.
type GitHubHandler struct {
	Config *config.Config
	// Instances tells which GitHub instances exist; nil means the
	// config's alone.
	Instances *instances.Registry
	// Secrets maps an instance name to the review App's webhook secret; an
	// instance without one is not connected, and its webhooks are refused.
	Secrets *syncmap.Map[string]
	Queue   Queue
	// Repos finds the repositories switched on; nil means the config's.
	Repos Repositories
	// Commands keeps the commands found in pull request comments; Bots
	// tells which comments are the bots' own and what mention addresses the
	// review App. Without either, comments are ignored.
	Commands CommandStore
	Bots     Bots
	// Wake nudges the dispatcher after a new run or command was queued. May
	// be nil.
	Wake func()
	Log  *slog.Logger
}

// issueCommentEvent holds only the fields of an issue_comment event that are
// read.
type issueCommentEvent struct {
	Action string `json:"action"`
	Issue  struct {
		Number int64 `json:"number"`
		// PullRequest is present when the issue is a pull request.
		PullRequest *json.RawMessage `json:"pull_request"`
	} `json:"issue"`
	Comment struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
		User struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
		// PerformedViaGithubApp is set when an App wrote the comment on a
		// person's behalf.
		PerformedViaGithubApp *json.RawMessage `json:"performed_via_github_app"`
	} `json:"comment"`
	Repository struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// reviewCommentEvent holds only the fields of a pull_request_review_comment
// event that are read.
type reviewCommentEvent struct {
	Action      string `json:"action"`
	PullRequest struct {
		Number int64 `json:"number"`
	} `json:"pull_request"`
	Comment struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
		User struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
		// InReplyToID is the ID of the thread's first comment, 0 (or absent)
		// for a comment that starts a thread.
		InReplyToID           int64            `json:"in_reply_to_id"`
		PerformedViaGithubApp *json.RawMessage `json:"performed_via_github_app"`
	} `json:"comment"`
	Repository struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// pullRequestEvent holds only the fields of a pull_request event that are read.
type pullRequestEvent struct {
	Action      string `json:"action"`
	PullRequest struct {
		Number int64 `json:"number"`
		Draft  bool  `json:"draft"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	} `json:"pull_request"`
	Repository struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	} `json:"repository"`
	// Changes lists what an "edited" event changed. Base is set only when the
	// pull request was retargeted; a title or body edit leaves it nil.
	Changes struct {
		Base *struct{} `json:"base"`
	} `json:"changes"`
}

// registry is Instances, or the config's instances alone when it is nil.
func (h *GitHubHandler) registry() *instances.Registry {
	if h.Instances != nil {
		return h.Instances
	}
	return instances.New(h.Config, nil, nil)
}

// secret is the webhook secret of instance name, "" when it has none.
func (h *GitHubHandler) secret(name string) string {
	s, _ := h.Secrets.Get(name)
	return s
}

func (h *GitHubHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := r.PathValue(instancePath)
	log := h.logger().With("instance", name)

	if platform, _ := h.registry().Platform(name); platform != config.PlatformGitHub {
		log.Info("webhook rejected", "outcome", "unknown_instance")
		http.Error(w, "unknown instance", http.StatusNotFound)
		return
	}

	// The signature covers the raw body, so the body is read first.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			log.Warn("webhook rejected", "outcome", "body_too_large")
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		log.Warn("webhook rejected", "outcome", "read_error")
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	if !signatureMatches(h.secret(name), body, r.Header.Get(githubSignatureHeader)) {
		log.Warn("webhook rejected", "outcome", "bad_signature")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	event := r.Header.Get(githubEventHeader)
	log = log.With("event", event)
	if event != githubPullRequest && event != githubIssueComment && event != githubReviewComment {
		// Includes "ping", sent when the webhook is created.
		log.Info("webhook handled", "outcome", outcomeIgnore, "reason", "other_event")
		w.WriteHeader(http.StatusOK)
		return
	}

	if event == githubIssueComment {
		h.comment(w, r, log, name, body)
		return
	}
	if event == githubReviewComment {
		h.reviewComment(w, r, log, name, body)
		return
	}
	var ev pullRequestEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		unusable(w, log, "malformed_json")
		return
	}
	pr := ev.PullRequest
	log = log.With("repository", ev.Repository.FullName, "number", pr.Number, "action", ev.Action)
	repo, ok, err := lookup(r.Context(), repositories(h.Repos, h.Config, h.registry()), config.PlatformGitHub, name, ev.Repository.FullName)
	if err != nil {
		log.Error("webhook failed", "outcome", "lookup_error", "err", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !ok {
		log.Info("webhook handled", "outcome", outcomeIgnore, "reason", "unknown_repository")
		w.WriteHeader(http.StatusOK)
		return
	}

	project := repo.Project
	republish := false
	switch ev.Action {
	case "opened", "reopened", "synchronize":
	case "edited":
		// Only a retarget changes what the head is reviewed against. A title
		// or body edit changes nothing.
		if ev.Changes.Base == nil {
			log.Info("webhook handled", "outcome", outcomeIgnore, "reason", "not_a_retarget")
			w.WriteHeader(http.StatusOK)
			return
		}
	case "closed":
	case "ready_for_review":
		republish = true
	default:
		log.Info("webhook handled", "outcome", outcomeIgnore, "reason", "action")
		w.WriteHeader(http.StatusOK)
		return
	}

	if ev.Repository.ID <= 0 || pr.Number <= 0 {
		unusable(w, log, "missing_ids")
		return
	}
	mr := store.MRKey{Instance: name, ProjectID: ev.Repository.ID, IID: pr.Number}

	if ev.Action == "closed" {
		n, err := h.Queue.SupersedeMR(r.Context(), mr)
		if err != nil {
			log.Error("webhook failed", "outcome", "supersede_error", "err", err)
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		log.Info("webhook handled", "outcome", "superseded", "runs", n)
		w.WriteHeader(http.StatusOK)
		return
	}

	head := pr.Head.SHA
	if !validSHA(head) {
		unusable(w, log, "bad_head_sha")
		return
	}

	// A draft is not reviewed while the repository doesn't review drafts. It
	// is reviewed when it is marked ready, which is handled below. Its runs
	// of an older head or target are superseded, as a new push's run would
	// have done, so none is shown or posted as the current review.
	if pr.Draft && !repo.ReviewDrafts {
		n, err := h.Queue.SupersedeDraftMR(r.Context(), mr, head, pr.Base.Ref)
		if err != nil {
			log.Error("webhook failed", "outcome", "supersede_error", "err", err)
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		log.Info("webhook handled", "outcome", "draft_skipped", "runs", n)
		w.WriteHeader(http.StatusOK)
		return
	}
	if republish {
		ok, err := h.Queue.Republish(r.Context(), mr, head, pr.Base.Ref)
		if err != nil {
			log.Error("webhook failed", "outcome", "republish_error", "err", err)
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		if ok {
			if h.Wake != nil {
				h.Wake()
			}
			log.Info("webhook handled", "outcome", "marked_ready", "result", "republished")
			w.WriteHeader(http.StatusOK)
			return
		}
		// The head has no review to decide again (none was queued while it
		// was a draft): review it now. A run already queued or running for
		// this head is kept.
	}
	id, created, err := h.Queue.EnqueueRun(r.Context(), mr, project, head, pr.Base.Ref)
	if err != nil {
		log.Error("webhook failed", "outcome", "enqueue_error", "err", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if created && h.Wake != nil {
		h.Wake()
	}
	if republish {
		log.Info("webhook handled", "outcome", "marked_ready", "result", "enqueued", "run", id, "created", created)
	} else {
		log.Info("webhook handled", "outcome", "enqueued", "run", id, "created", created)
	}
	w.WriteHeader(http.StatusOK)
}

// comment handles an issue_comment event: a new comment by a person on a pull
// request may carry a command, which is stored for the engine. Nothing here
// calls GitHub.
func (h *GitHubHandler) comment(w http.ResponseWriter, r *http.Request, log *slog.Logger, instance string, body []byte) {
	var ev issueCommentEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		unusable(w, log, "malformed_json")
		return
	}
	c := ev.Comment
	switch {
	case ev.Action != "created":
		// An edit must not run a command again, nor a deletion run one.
		log.Info("webhook handled", "outcome", outcomeIgnore, "reason", "action")
	case ev.Issue.PullRequest == nil || string(*ev.Issue.PullRequest) == "null":
		log.Info("webhook handled", "outcome", outcomeIgnore, "reason", "not_a_pull_request")
	default:
		outcome, reason, err := commands{Bots: h.Bots, Store: h.Commands,
			Repos: repositories(h.Repos, h.Config, h.registry()), Wake: h.Wake}.receive(r.Context(), comment{
			Platform: config.PlatformGitHub, Instance: instance, Project: ev.Repository.FullName,
			ProjectID: ev.Repository.ID, IID: ev.Issue.Number, CommentID: c.ID,
			Author: c.User.Login, AuthorID: c.User.ID, AuthorIsBot: c.User.Type == "Bot" || viaApp(c.PerformedViaGithubApp), Body: c.Body,
		})
		log = log.With("repository", ev.Repository.FullName, "number", ev.Issue.Number)
		if err != nil {
			log.Error("webhook failed", "outcome", outcome, "err", err)
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		log.Info("webhook handled", "outcome", outcome, "reason", reason)
	}
	w.WriteHeader(http.StatusOK)
}

// reviewComment handles a pull_request_review_comment event: a new reply by a
// person in a thread on a pull request may carry a command (dismissing the
// finding the thread is about), which is stored for the engine. A comment that
// starts a thread is not a reply to anything and is ignored. Nothing here
// calls GitHub.
func (h *GitHubHandler) reviewComment(w http.ResponseWriter, r *http.Request, log *slog.Logger, instance string, body []byte) {
	var ev reviewCommentEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		unusable(w, log, "malformed_json")
		return
	}
	c := ev.Comment
	switch {
	case ev.Action != "created":
		log.Info("webhook handled", "outcome", outcomeIgnore, "reason", "action")
	case c.InReplyToID == 0:
		log.Info("webhook handled", "outcome", outcomeIgnore, "reason", "not_a_reply")
	default:
		outcome, reason, err := commands{Bots: h.Bots, Store: h.Commands,
			Repos: repositories(h.Repos, h.Config, h.registry()), Wake: h.Wake}.receive(r.Context(), comment{
			Platform: config.PlatformGitHub, Instance: instance, Project: ev.Repository.FullName,
			ProjectID: ev.Repository.ID, IID: ev.PullRequest.Number, CommentID: c.ID,
			Author: c.User.Login, AuthorID: c.User.ID, AuthorIsBot: c.User.Type == "Bot" || viaApp(c.PerformedViaGithubApp), Body: c.Body,
			ReplyTo: c.InReplyToID,
		})
		log = log.With("repository", ev.Repository.FullName, "number", ev.PullRequest.Number)
		if err != nil {
			log.Error("webhook failed", "outcome", outcome, "err", err)
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		log.Info("webhook handled", "outcome", outcome, "reason", reason)
	}
	w.WriteHeader(http.StatusOK)
}

// viaApp reports whether performed_via_github_app names an App (it is null
// for a comment written by hand).
func viaApp(raw *json.RawMessage) bool {
	return raw != nil && string(*raw) != "null"
}

func (h *GitHubHandler) logger() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// signatureMatches checks an X-Hub-Signature-256 header ("sha256=<hex>")
// against the HMAC-SHA256 of the raw body, in constant time. An unset secret
// never matches.
func signatureMatches(secret string, body []byte, header string) bool {
	if secret == "" {
		return false
	}
	hexSum, ok := strings.CutPrefix(header, githubSignaturePrefix)
	if !ok {
		return false
	}
	got, err := hex.DecodeString(hexSum)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), got)
}

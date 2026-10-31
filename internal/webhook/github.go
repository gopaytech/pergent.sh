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
)

// GitHubHandler serves POST /webhooks/github/{instance}: the review App's
// pull_request webhook. Like the GitLab handler it authenticates the event,
// queues work and answers fast; the engine does the rest.
type GitHubHandler struct {
	Config *config.Config
	// Instances tells which GitHub instances exist.
	Instances *instances.Registry
	// Secrets maps an instance name to the review App's webhook secret; an
	// instance without one is not connected, and its webhooks are refused.
	Secrets *syncmap.Map[string]
	Queue   Queue
	// Repos finds the repositories switched on; nil means the config's.
	Repos Repositories
	// Wake nudges the dispatcher after a new run was queued. May be nil.
	Wake func()
	Log  *slog.Logger
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

	if platform, _ := h.Instances.Platform(name); platform != config.PlatformGitHub {
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
	if event != githubPullRequest {
		// Includes "ping", sent when the webhook is created.
		log.Info("webhook handled", "outcome", outcomeIgnore, "reason", "other_event")
		w.WriteHeader(http.StatusOK)
		return
	}

	var ev pullRequestEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		unusable(w, log, "malformed_json")
		return
	}
	pr := ev.PullRequest
	log = log.With("repository", ev.Repository.FullName, "number", pr.Number, "action", ev.Action)
	repo, ok, err := lookup(r.Context(), repositories(h.Repos, h.Config, h.Instances), config.PlatformGitHub, name, ev.Repository.FullName)
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

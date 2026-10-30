// Package webhook receives GitLab webhooks. It authenticates them, turns
// merge request events into queued review runs and replies fast; the engine
// does the work.
package webhook

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	maxBodyBytes  = 5 << 20
	mrHookEvent   = "Merge Request Hook"
	tokenHeader   = "X-Gitlab-Token"
	eventHeader   = "X-Gitlab-Event"
	mrObjectKind  = "merge_request"
	instancePath  = "instance"
	outcomeIgnore = "ignored"
)

// Queue is the part of the store the handler needs.
type Queue interface {
	EnqueueRun(ctx context.Context, mr store.MRKey, projectPath, headSHA, targetBranch string) (string, bool, error)
	SupersedeMR(ctx context.Context, mr store.MRKey) (int, error)
}

// Handler serves POST /webhooks/gitlab/{instance}.
type Handler struct {
	Config *config.Config
	// Secrets maps an instance name to its webhook secret.
	Secrets map[string]string
	Queue   Queue
	// Wake nudges the dispatcher after a new run was queued. May be nil.
	Wake func()
	Log  *slog.Logger
}

// mergeRequestEvent holds only the fields of a Merge Request Hook that are read.
type mergeRequestEvent struct {
	ObjectKind string `json:"object_kind"`
	Project    struct {
		ID                int64  `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	ObjectAttributes struct {
		IID          int64  `json:"iid"`
		Action       string `json:"action"`
		OldRev       string `json:"oldrev"`
		TargetBranch string `json:"target_branch"`
		LastCommit   struct {
			ID string `json:"id"`
		} `json:"last_commit"`
	} `json:"object_attributes"`
	Changes struct {
		TargetBranch *struct {
			Previous string `json:"previous"`
			Current  string `json:"current"`
		} `json:"target_branch"`
	} `json:"changes"`
}

// retargeted reports whether an update event changed the target branch.
func (ev *mergeRequestEvent) retargeted() bool {
	tb := ev.Changes.TargetBranch
	return tb != nil && tb.Previous != tb.Current
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	log := h.logger().With("instance", r.PathValue(instancePath))

	name := r.PathValue(instancePath)
	if _, ok := h.Config.Instance(name); !ok {
		log.Info("webhook rejected", "outcome", "unknown_instance")
		http.Error(w, "unknown instance", http.StatusNotFound)
		return
	}
	if !tokenMatches(h.Secrets[name], r.Header.Get(tokenHeader)) {
		log.Warn("webhook rejected", "outcome", "bad_token")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	event := r.Header.Get(eventHeader)
	log = log.With("event", event)
	if event != mrHookEvent {
		log.Info("webhook handled", "outcome", outcomeIgnore, "reason", "other_event")
		w.WriteHeader(http.StatusOK)
		return
	}

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
	var ev mergeRequestEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		log.Warn("webhook rejected", "outcome", "malformed_json")
		http.Error(w, "malformed json", http.StatusBadRequest)
		return
	}
	if ev.ObjectKind != mrObjectKind {
		log.Info("webhook handled", "outcome", outcomeIgnore, "reason", "other_object_kind")
		w.WriteHeader(http.StatusOK)
		return
	}

	attrs := ev.ObjectAttributes
	project, ok := h.project(name, ev.Project.PathWithNamespace)
	log = log.With("project", ev.Project.PathWithNamespace, "iid", attrs.IID, "action", attrs.Action)
	if !ok {
		log.Info("webhook handled", "outcome", outcomeIgnore, "reason", "unknown_project")
		w.WriteHeader(http.StatusOK)
		return
	}

	switch attrs.Action {
	case "open", "reopen":
	case "update":
		// A push carries oldrev. A retarget doesn't, but changes the base
		// the head is reviewed against. Any other edit changes neither.
		if attrs.OldRev == "" && !ev.retargeted() {
			log.Info("webhook handled", "outcome", outcomeIgnore, "reason", "not_a_push")
			w.WriteHeader(http.StatusOK)
			return
		}
	case "close", "merge":
	default:
		log.Info("webhook handled", "outcome", outcomeIgnore, "reason", "action")
		w.WriteHeader(http.StatusOK)
		return
	}

	if ev.Project.ID <= 0 || attrs.IID <= 0 {
		log.Warn("webhook rejected", "outcome", "missing_ids")
		http.Error(w, "missing project id or iid", http.StatusBadRequest)
		return
	}
	mr := store.MRKey{Instance: name, ProjectID: ev.Project.ID, IID: attrs.IID}

	if attrs.Action == "close" || attrs.Action == "merge" {
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

	head := attrs.LastCommit.ID
	if !validSHA(head) {
		log.Warn("webhook rejected", "outcome", "bad_head_sha")
		http.Error(w, "bad head sha", http.StatusBadRequest)
		return
	}
	id, created, err := h.Queue.EnqueueRun(r.Context(), mr, project, head, attrs.TargetBranch)
	if err != nil {
		log.Error("webhook failed", "outcome", "enqueue_error", "err", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if created && h.Wake != nil {
		h.Wake()
	}
	log.Info("webhook handled", "outcome", "enqueued", "run", id, "created", created)
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) logger() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// project returns the configured spelling of a project on an instance.
// GitLab paths are case-insensitive.
func (h *Handler) project(instance, path string) (string, bool) {
	for _, repo := range h.Config.Repositories {
		if repo.GitLab == instance && strings.EqualFold(repo.Project, path) {
			return repo.Project, true
		}
	}
	return "", false
}

// tokenMatches compares in constant time. Both sides are hashed first so the
// length of the secret does not leak. An unset secret never matches.
func tokenMatches(secret, got string) bool {
	if secret == "" {
		return false
	}
	a := sha256.Sum256([]byte(secret))
	b := sha256.Sum256([]byte(got))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// validSHA reports whether s is a full lowercase hex object id (sha1 or sha256).
func validSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

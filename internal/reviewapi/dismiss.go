package reviewapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"unicode"
	"unicode/utf8"

	"github.com/gopaytech/pergent.sh/internal/actions"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// maxDismissBody caps the body of a dismissal: the reason is at most 500
// characters, so this holds it in any encoding.
const maxDismissBody = 16 << 10

// What a dismissal answers with.
const (
	DismissedStatus        = "dismissed"
	AlreadyDismissedStatus = "already_dismissed"
	RestoredStatus         = "restored"
	NotDismissedStatus     = "not_dismissed"
)

// Why a dismissal or a restore is refused. The messages start lowercase and
// are safe to show.
var (
	ErrBadReason     = fmt.Errorf("a dismissal needs a reason of at most %d characters", actions.MaxReasonRunes)
	ErrNotFinished   = errors.New("findings can be dismissed once the review is completed")
	ErrNoSuchFinding = errors.New("finding not found")
	ErrReasonControl = errors.New("the reason must not hold a control character")
)

// DismissRequest is the body of POST /v1/reviews/{id}/findings/{finding}/dismiss.
type DismissRequest struct {
	Reason string `json:"reason"`
}

// DismissResult is the body of the answer to a dismissal or a restore.
type DismissResult struct {
	Status string `json:"status"`
}

// DismissStore is the part of the store a dismissal needs. *store.Store
// satisfies it.
type DismissStore interface {
	DismissAPIFinding(ctx context.Context, d store.APIDismissal, runID string) (created bool, err error)
	UndismissAPIFinding(ctx context.Context, clientID int64, tenant, subject, fingerprint, runID, actor string) (bool, error)
}

// CheckReason returns the reason as it is stored (without images, trimmed), or
// ErrBadReason when that is empty or longer than actions.MaxReasonRunes
// characters. A control character other than a line break or a tab is refused:
// a NUL would not even be stored.
func CheckReason(reason string) (string, error) {
	reason = actions.CleanReason(reason)
	if n := utf8.RuneCountInString(reason); n < 1 || n > actions.MaxReasonRunes {
		return "", ErrBadReason
	}
	for _, c := range reason {
		if unicode.IsControl(c) && c != '\n' && c != '\r' && c != '\t' {
			return "", ErrReasonControl
		}
	}
	return reason, nil
}

// storedFinding is the finding id of the completed review rv, and the name of
// the skill that found it. A review that is not completed is ErrNotFinished,
// and an id the stored result does not hold is ErrNoSuchFinding. So is a
// finding with no fingerprint: a dismissal is kept by fingerprint, so every
// such finding of the subject would share it.
func storedFinding(rv store.APIReview, id string) (skill string, f Finding, err error) {
	if StatusOf(rv) != StatusCompleted || len(rv.Response) == 0 {
		return "", Finding{}, ErrNotFinished
	}
	var body Review
	if err := json.Unmarshal(rv.Response, &body); err != nil {
		return "", Finding{}, fmt.Errorf("the stored result of review %s is not readable: %w", rv.Run.ID, err)
	}
	for _, s := range body.Skills {
		for _, f := range s.Findings {
			if f.ID == id && f.Fingerprint != "" {
				return s.Name, f, nil
			}
		}
	}
	return "", Finding{}, ErrNoSuchFinding
}

// DismissFinding sets finding id of the review rv aside for its client,
// tenant and subject, as actor, so later reviews of the subject honour it.
// The review itself is not changed. created is false when the finding was
// dismissed already.
func DismissFinding(ctx context.Context, st DismissStore, rv store.APIReview, id, reason, actor string) (created bool, err error) {
	reason, err = CheckReason(reason)
	if err != nil {
		return false, err
	}
	skill, f, err := storedFinding(rv, id)
	if err != nil {
		return false, err
	}
	// Anything but a suggestion is dismissed as a blocking finding, as for a
	// merge request.
	severity := "blocking"
	if f.Severity == "suggestion" {
		severity = "suggestion"
	}
	return st.DismissAPIFinding(ctx, store.APIDismissal{
		ClientID: rv.ClientID, Tenant: rv.Tenant, Subject: rv.Subject, Fingerprint: f.Fingerprint,
		Severity: severity, Skill: skill, File: f.File, Title: f.Title, Reason: reason, Actor: actor,
	}, rv.Run.ID)
}

// RestoreFinding takes back the dismissal of finding id of the review rv, as
// actor. restored is false when it was not dismissed.
func RestoreFinding(ctx context.Context, st DismissStore, rv store.APIReview, id, actor string) (restored bool, err error) {
	_, f, err := storedFinding(rv, id)
	if err != nil {
		return false, err
	}
	return st.UndismissAPIFinding(ctx, rv.ClientID, rv.Tenant, rv.Subject, f.Fingerprint, rv.Run.ID, actor)
}

// refuse answers a dismissal that DismissFinding or RestoreFinding refused, or
// failed. It returns false for an error that is not a refusal.
func refuse(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, ErrBadReason), errors.Is(err, ErrReasonControl):
		writeError(w, http.StatusUnprocessableEntity, CodeInvalid, err.Error())
	case errors.Is(err, ErrNotFinished):
		writeError(w, http.StatusConflict, CodeNotFinished, err.Error())
	case errors.Is(err, ErrNoSuchFinding):
		writeError(w, http.StatusNotFound, CodeNotFound, err.Error())
	default:
		return false
	}
	return true
}

// dismissFinding is POST /v1/reviews/{id}/findings/{finding}/dismiss. The
// review is read as GET reads it, so another client's review is not found, and
// that comes before anything about the body is judged.
func (h *Handler) dismissFinding(w http.ResponseWriter, r *http.Request) {
	rv, ok := h.reviewOf(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxDismissBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, CodeTooLarge, fmt.Sprintf("the request is over %d bytes", maxDismissBody))
			return
		}
		writeError(w, http.StatusUnprocessableEntity, CodeInvalid, "the body could not be read")
		return
	}
	if !validText(body) {
		writeError(w, http.StatusUnprocessableEntity, CodeInvalid, notUTF8Message)
		return
	}
	var req DismissRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, CodeInvalid, decodeMessage(err))
		return
	}
	created, err := DismissFinding(r.Context(), h.Store, rv, r.PathValue("finding"), req.Reason, "key "+keyOf(r).Name)
	if err != nil {
		if !refuse(w, err) {
			h.log().Error("review API: dismissing a finding failed", "client_id", rv.ClientID, "review", rv.Run.ID, "err", err)
			writeInternal(w)
		}
		return
	}
	status := DismissedStatus
	if !created {
		status = AlreadyDismissedStatus
	}
	writeJSON(w, http.StatusOK, DismissResult{Status: status})
}

// restoreFinding is DELETE on the same path.
func (h *Handler) restoreFinding(w http.ResponseWriter, r *http.Request) {
	rv, ok := h.reviewOf(w, r)
	if !ok {
		return
	}
	restored, err := RestoreFinding(r.Context(), h.Store, rv, r.PathValue("finding"), "key "+keyOf(r).Name)
	if err != nil {
		if !refuse(w, err) {
			h.log().Error("review API: restoring a finding failed", "client_id", rv.ClientID, "review", rv.Run.ID, "err", err)
			writeInternal(w)
		}
		return
	}
	status := RestoredStatus
	if !restored {
		status = NotDismissedStatus
	}
	writeJSON(w, http.StatusOK, DismissResult{Status: status})
}

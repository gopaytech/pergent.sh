package callback

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
)

// PanelResults is the part of the engine the panel route needs.
type PanelResults interface {
	// PanelMemberResult returns the stored result of one member of the
	// panel the run belongs to. The bool is false for a wrong token, a role
	// that is not a member, an unknown run or a member with no result.
	PanelMemberResult(ctx context.Context, runID, token, role string) ([]byte, bool, error)
}

// PanelHandler serves GET /runs/{id}/panel/{role}: a panel's merge Job
// reads member a's or b's result with its own result token. Every refusal
// is the same 404, so the answer never tells whether a run or a result
// exists. The token is not spent. The body and the token are never logged.
type PanelHandler struct {
	Panel PanelResults
	Log   *slog.Logger
}

func (h *PanelHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, role := r.PathValue("id"), r.PathValue("role")
	token, ok := bearer(r.Header.Get("Authorization"))
	if !ok || (role != "a" && role != "b") {
		http.NotFound(w, r)
		return
	}
	body, found, err := h.Panel.PanelMemberResult(r.Context(), id, token, role)
	if err != nil {
		logOr(h.Log).Error("panel: serving a member result failed", "run", id, "role", role, "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

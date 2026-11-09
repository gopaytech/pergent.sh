package dashboard

import (
	"net/http"
	"strings"
	"unicode"

	"github.com/gopaytech/pergent.sh/internal/policy"
)

// maxFooterNote is the most bytes the footer's text may have: a line, not a
// section.
const maxFooterNote = 200

func (d *Dashboard) commentFooterView(r *http.Request) (CommentFooterView, error) {
	note, err := d.Store.CommentFooterNote(r.Context())
	return CommentFooterView{Note: note, Default: policy.DefaultFooterNote}, err
}

func (d *Dashboard) getCommentFooter(w http.ResponseWriter, r *http.Request) {
	v, err := d.commentFooterView(r)
	if err != nil {
		d.failed(w, r, "reading the comment footer", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (d *Dashboard) putCommentFooter(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Note *string `json:"note"`
	}
	if err := decodeStrict(r, &body); err != nil || body.Note == nil {
		writeError(w, http.StatusBadRequest, "The body is {\"note\": \"...\"}.")
		return
	}
	note, msg := checkFooterNote(*body.Note)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	ctx, se := r.Context(), session(r)
	if err := d.Store.PutCommentFooterNote(ctx, note, d.now()); err != nil {
		d.Log.Error("dashboard: saving the comment footer failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	d.Log.Info("dashboard: comment footer updated", "by", se.Username)
	d.audit(ctx, se, "comment_footer_updated", "", "", note)
	v, err := d.commentFooterView(r)
	if err != nil {
		d.failed(w, r, "reading the comment footer", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// checkFooterNote returns the text to store, or why it can't be. The text goes
// into every review comment as markdown, one line of it. An HTML tag could
// open a comment and swallow the hidden findings block after it, and an image
// would make every reader's browser fetch it.
func checkFooterNote(note string) (string, string) {
	note = strings.TrimSpace(note)
	switch {
	case len(note) > maxFooterNote:
		return "", "The footer text is at most 200 bytes."
	case strings.IndexFunc(note, unicode.IsControl) >= 0:
		return "", "The footer text is one line."
	case strings.Contains(note, "<"):
		return "", "The footer text can't contain <."
	case strings.Contains(note, "!["):
		return "", "The footer text can't contain an image."
	}
	return note, ""
}

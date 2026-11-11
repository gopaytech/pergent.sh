package reviewapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"unicode"
	"unicode/utf8"

	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// The limits of a request's fields. The files have their own (CheckFiles, and
// the client's max_files and max_request_bytes).
const (
	maxSubject        = 200
	maxTenant         = 100
	maxTitle          = 200
	maxDescription    = 32 << 10
	maxIdempotencyKey = 200
)

const (
	keyInvalidMessage    = "Idempotency-Key must be 1 to 200 printable ASCII characters"
	keyReusedMessage     = "the Idempotency-Key was used before for another request"
	reviewNotFoundMsg    = "review not found"
	noSkillsMessage      = "this client has no skills: an admin chooses them in its settings"
	modelGoneMessage     = "this client's model is no longer in the model catalog"
	notJSONMessage       = "the body is not valid JSON"
	notUTF8Message       = "the body is not UTF-8 text"
	notAnObjectMessage   = "the body must be a JSON object"
	idempotencyKeyHeader = "Idempotency-Key"
)

// Request is the body of POST /v1/reviews.
type Request struct {
	Subject     string `json:"subject"`
	Tenant      string `json:"tenant"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Files       []File `json:"files"`
}

// Sealed is what Box.Seal turns into a review's inputs_sealed: the fast-import
// stream and the description. Both go with the files when the Job has them;
// only the title stays.
type Sealed struct {
	Import      []byte `json:"import"`
	Description string `json:"description"`
}

// createReview is POST /v1/reviews. The order of its checks is part of the
// API: a retry of a request is answered from what was stored before the body
// is judged, and a request that can't be reviewed is refused before anything
// is queued.
func (h *Handler) createReview(w http.ResponseWriter, r *http.Request) {
	client := clientOf(r)

	limit := min(client.Limits.MaxRequestBytes, store.HardMaxRequestBytes)
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(limit)))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, CodeTooLarge, fmt.Sprintf("the request is over %d bytes", limit))
			return
		}
		writeError(w, http.StatusUnprocessableEntity, CodeInvalid, "the body could not be read")
		return
	}
	sum := sha256.Sum256(body)

	idemKey, ok := idempotencyKey(r)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, CodeInvalid, keyInvalidMessage)
		return
	}
	if idemKey != "" {
		prior, hash, found, err := h.Store.APIReviewByKey(r.Context(), client.ID, idemKey)
		if err != nil {
			h.log().Error("review API: looking up an idempotency key failed", "client_id", client.ID, "err", err)
			writeInternal(w)
			return
		}
		if found {
			if !bytes.Equal(hash, sum[:]) {
				writeError(w, http.StatusUnprocessableEntity, CodeIdempotencyKeyReused, keyReusedMessage)
				return
			}
			writeReview(w, http.StatusAccepted, prior)
			return
		}
	}

	// encoding/json turns bytes that are not UTF-8 into U+FFFD without an
	// error, which would review text the caller never sent.
	if !validText(body) {
		writeError(w, http.StatusUnprocessableEntity, CodeInvalid, notUTF8Message)
		return
	}
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, CodeInvalid, decodeMessage(err))
		return
	}
	if msg := checkRequest(req); msg != "" {
		writeError(w, http.StatusUnprocessableEntity, CodeInvalid, msg)
		return
	}
	if err := CheckFiles(req.Files); err != nil {
		writeError(w, http.StatusUnprocessableEntity, CodeInvalid, err.Error())
		return
	}

	if len(req.Files) > client.Limits.MaxFiles {
		writeError(w, http.StatusRequestEntityTooLarge, CodeTooLarge,
			fmt.Sprintf("the request holds more than %d files", client.Limits.MaxFiles))
		return
	}
	im, err := BuildImport(req.Files)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, CodeInvalid, err.Error())
		return
	}
	// The Job gets the stream in a Secret, which holds so much and no more.
	if len(im.Stream) > kube.FilesSecretMaxBytes {
		writeError(w, http.StatusRequestEntityTooLarge, CodeTooLarge,
			fmt.Sprintf("the files are over %d bytes once packed for the review", kube.FilesSecretMaxBytes))
		return
	}

	if msg, err := h.cannotReview(r, client); err != nil {
		h.log().Error("review API: checking a client's skills failed", "client_id", client.ID, "err", err)
		writeInternal(w)
		return
	} else if msg != "" {
		writeError(w, http.StatusConflict, CodeCannotReview, msg)
		return
	}

	plain, err := json.Marshal(Sealed{Import: im.Stream, Description: req.Description})
	if err != nil {
		h.log().Error("review API: packing a request failed", "client_id", client.ID, "err", err)
		writeInternal(w)
		return
	}
	id, created, err := h.Store.EnqueueAPIReview(r.Context(), store.NewAPIReview{
		ClientID: client.ID, Tenant: req.Tenant, Subject: req.Subject, Title: req.Title,
		KeyName: keyOf(r).Name, IdempotencyKey: idemKey, RequestHash: sum[:],
		BaseSHA: im.Base, HeadSHA: im.Head, InputsSealed: h.Box.Seal(plain),
		FileCount: len(req.Files), FileBytes: im.Bytes,
	})
	if errors.Is(err, store.ErrIdempotencyMismatch) {
		// A retry that raced the first request.
		writeError(w, http.StatusUnprocessableEntity, CodeIdempotencyKeyReused, keyReusedMessage)
		return
	}
	if err != nil {
		h.log().Error("review API: queueing a review failed", "client_id", client.ID, "err", err)
		writeInternal(w)
		return
	}
	if created && h.Wake != nil {
		h.Wake()
	}
	rv, found, err := h.Store.APIReview(r.Context(), client.ID, id)
	if err != nil || !found {
		h.log().Error("review API: reading a queued review failed", "client_id", client.ID, "review", id, "found", found, "err", err)
		if !created {
			writeInternal(w)
			return
		}
		// The review is queued and woken: a 500 would invite a retry that
		// supersedes it. Answer with what is known.
		rv = store.APIReview{
			Run: store.Run{ID: id, State: store.Queued}, ClientID: client.ID, Tenant: req.Tenant,
			Subject: req.Subject, Title: req.Title, CreatedAt: h.now(),
		}
	}
	writeReview(w, http.StatusAccepted, rv)
}

// getReview is GET /v1/reviews/{id}. A review of another client, an unknown ID
// and a malformed one get one answer, so a caller learns nothing about IDs it
// does not own.
func (h *Handler) getReview(w http.ResponseWriter, r *http.Request) {
	rv, found, err := h.Store.APIReview(r.Context(), clientOf(r).ID, r.PathValue("id"))
	if err != nil {
		h.log().Error("review API: reading a review failed", "client_id", clientOf(r).ID, "err", err)
		writeInternal(w)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, CodeNotFound, reviewNotFoundMsg)
		return
	}
	writeReview(w, http.StatusOK, rv)
}

// Body is the body of GET /v1/reviews/{id} for rv, which its callback posts
// too: the stored response as it is stored or, before there is one, the
// pending body, as writeJSON writes it.
func Body(rv store.APIReview) ([]byte, error) {
	if len(rv.Response) > 0 {
		return rv.Response, nil
	}
	b, err := json.Marshal(Pending(rv))
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// writeReview answers with the review's Body.
func writeReview(w http.ResponseWriter, status int, rv store.APIReview) {
	w.Header().Set("Location", "/v1/reviews/"+rv.Run.ID)
	body, err := Body(rv)
	if err != nil {
		writeInternal(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// cannotReview is why the client's reviews can't run now, or "".
func (h *Handler) cannotReview(r *http.Request, client store.APIClient) (string, error) {
	if len(client.Skills) == 0 {
		return noSkillsMessage, nil
	}
	lib, err := h.Store.ResolveLibrary(r.Context(), client.Skills)
	if err != nil {
		return "", err
	}
	if len(lib.Missing) > 0 {
		return fmt.Sprintf("skill %q is not in the skill library", lib.Missing[0]), nil
	}
	ok := false
	if client.ModelID == "" {
		_, ok = h.Models.DefaultModel()
	} else {
		_, ok = h.Models.ModelByID(client.ModelID)
	}
	if !ok {
		return modelGoneMessage, nil
	}
	return "", nil
}

// idempotencyKey reads the Idempotency-Key header: "" when there is none, and
// ok false when it is not one key of 1 to 200 printable ASCII characters.
func idempotencyKey(r *http.Request) (key string, ok bool) {
	vals := r.Header.Values(idempotencyKeyHeader)
	if len(vals) == 0 {
		return "", true
	}
	key = vals[0]
	if len(vals) > 1 || key == "" || len(key) > maxIdempotencyKey {
		return "", false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x20 || key[i] > 0x7e {
			return "", false
		}
	}
	return key, true
}

// decodeMessage is what is wrong with a body that did not decode. A field name
// comes from the request type, never from the body.
func decodeMessage(err error) string {
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		if te.Field == "" {
			return notAnObjectMessage
		}
		return te.Field + " has the wrong type"
	}
	return notJSONMessage
}

// checkRequest applies the rules of the fields other than the files. It
// returns the broken rule, or "".
func checkRequest(req Request) string {
	n := utf8.RuneCountInString(req.Subject)
	if n < 1 || n > maxSubject {
		return fmt.Sprintf("subject must be 1 to %d characters", maxSubject)
	}
	if utf8.RuneCountInString(req.Tenant) > maxTenant {
		return fmt.Sprintf("tenant must be at most %d characters", maxTenant)
	}
	if utf8.RuneCountInString(req.Title) > maxTitle {
		return fmt.Sprintf("title must be at most %d characters", maxTitle)
	}
	for _, f := range []struct{ name, value string }{{"subject", req.Subject}, {"tenant", req.Tenant}, {"title", req.Title}} {
		for _, c := range f.value {
			if unicode.IsControl(c) {
				return f.name + " must not hold a control character"
			}
		}
	}
	if len(req.Description) > maxDescription {
		return fmt.Sprintf("description must be at most %d bytes", maxDescription)
	}
	return ""
}

// validText reports whether a JSON body is UTF-8 text all the way: no invalid
// byte, and no \uD800 to \uDFFF escape that is not half of a high and low
// pair. Both read as U+FFFD, with no error, once decoded.
func validText(body []byte) bool {
	if !utf8.Valid(body) {
		return false
	}
	hex4 := func(i int) (rune, bool) {
		if i+4 > len(body) {
			return 0, false
		}
		var v rune
		for _, c := range body[i : i+4] {
			switch {
			case c >= '0' && c <= '9':
				v = v<<4 | rune(c-'0')
			case c >= 'a' && c <= 'f':
				v = v<<4 | rune(c-'a'+10)
			case c >= 'A' && c <= 'F':
				v = v<<4 | rune(c-'A'+10)
			default:
				return 0, false
			}
		}
		return v, true
	}
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			continue
		}
		// The escape is two bytes (\\ is a backslash, not the start of \u).
		if i+1 >= len(body) || body[i+1] != 'u' {
			i++
			continue
		}
		v, ok := hex4(i + 2)
		switch {
		case !ok:
			// Not a valid escape: the decoder refuses the body.
		case v >= 0xdc00 && v <= 0xdfff:
			return false
		case v >= 0xd800 && v <= 0xdbff:
			if i+12 > len(body) || body[i+6] != '\\' || body[i+7] != 'u' {
				return false
			}
			if lo, ok := hex4(i + 8); !ok || lo < 0xdc00 || lo > 0xdfff {
				return false
			}
			i += 11
			continue
		}
		i += 5
	}
	return true
}

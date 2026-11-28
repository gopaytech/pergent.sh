package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gopaytech/pergent.sh/internal/config"
)

const (
	msgBadContextWindow = "contextWindow must be null or a whole number from 4096 to 10000000"
	// An entry holds both numbers or neither: opencode merges them over its
	// own, so an empty one would replace its known value with 0. The names
	// are the ones the page shows.
	msgWindowPair = "Set both Context window and Max output, or neither."
)

// maxOutputProblem is the 400 text for a bad maxOutput: its range ends at
// the context window when the entry has one.
func maxOutputProblem(contextWindow int) string {
	if contextWindow > 0 {
		return fmt.Sprintf("maxOutput must be null or a whole number from %d to %d, the entry's context window",
			config.MinMaxOutput, contextWindow)
	}
	return fmt.Sprintf("maxOutput must be null or a whole number from %d to %d", config.MinMaxOutput, config.MaxContextWindow)
}

// tokenField reads one body field. keep is true for an absent one (the
// value stays), null empties it (0), anything else must be digits only.
func tokenField(raw json.RawMessage) (n int, keep, ok bool) {
	if len(raw) == 0 {
		return 0, true, true
	}
	text := bytes.TrimSpace(raw)
	if string(text) == "null" {
		return 0, false, true
	}
	for _, c := range text {
		if c < '0' || c > '9' {
			return 0, false, false
		}
	}
	n, err := strconv.Atoi(string(text))
	if err != nil || n == 0 { // 0 is not a token count: null empties
		return 0, false, false
	}
	return n, false, true
}

// windowBody reads contextWindow and maxOutput of a request: an absent field
// keeps its before value, null empties it. The result is checked as a pair:
// each in its range (max output up to the context window) and both set or
// both empty. On a refusal it has written the 400 and ok is false.
func windowBody(w http.ResponseWriter, contextRaw, outputRaw json.RawMessage, beforeContext, beforeOutput int) (contextWindow, maxOutput int, ok bool) {
	contextWindow, maxOutput = beforeContext, beforeOutput
	n, keep, valid := tokenField(contextRaw)
	if !valid {
		writeError(w, http.StatusBadRequest, msgBadContextWindow)
		return 0, 0, false
	}
	if !keep {
		contextWindow = n
	}
	if n, keep, valid = tokenField(outputRaw); !valid {
		writeError(w, http.StatusBadRequest, maxOutputProblem(contextWindow))
		return 0, 0, false
	} else if !keep {
		maxOutput = n
	}
	if contextWindow != 0 && !config.ValidContextWindow(contextWindow) {
		writeError(w, http.StatusBadRequest, msgBadContextWindow)
		return 0, 0, false
	}
	if maxOutput != 0 && !config.ValidMaxOutput(maxOutput, contextWindow) {
		writeError(w, http.StatusBadRequest, maxOutputProblem(contextWindow))
		return 0, 0, false
	}
	// Ranges hold here, so config's check can only refuse a single value.
	if err := config.ValidModelWindow(config.TokenCount(contextWindow), config.TokenCount(maxOutput)); err != nil {
		writeError(w, http.StatusBadRequest, msgWindowPair)
		return 0, 0, false
	}
	return contextWindow, maxOutput, true
}

// windowChanges names what changed, for the audit entry and the log.
func windowChanges(beforeContext, beforeOutput, contextWindow, maxOutput int) []string {
	var out []string
	if beforeContext != contextWindow {
		out = append(out, "context_window "+tokenText(beforeContext)+" to "+tokenText(contextWindow))
	}
	if beforeOutput != maxOutput {
		out = append(out, "max_output "+tokenText(beforeOutput)+" to "+tokenText(maxOutput))
	}
	return out
}

func tokenText(n int) string {
	if n == 0 {
		return "none"
	}
	return strconv.Itoa(n)
}

// tokensOrNull is n for a view: null when the entry has none (0).
func tokensOrNull(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

// modelWindowOff is why Jobs get no context window or max output for now,
// "" when both images read them (config.ModelWindowImage).
func modelWindowOff(reviewImage, scanImage string) string {
	reviewOK, reviewWhy := config.ModelWindowImage(reviewImage)
	scanOK, scanWhy := config.ModelWindowImage(scanImage)
	switch {
	case reviewOK && scanOK:
		return ""
	case !reviewOK && !scanOK:
		return "Jobs get no context window or max output for now: " + reviewWhy + "."
	case !reviewOK:
		return "Reviews get no context window or max output for now: " + reviewWhy + "."
	default:
		return "Security scans get no context window or max output for now: " + scanWhy + "."
	}
}

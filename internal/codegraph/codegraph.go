// Package codegraph holds what the graph build Job and pergent.sh agree on
// (docs/concepts/code-graph.md): the shape of a build's error, and the fixed
// messages a build may end with. A build posts either a pergent-graph/v1 file
// or an ErrorBody. Only a message of this package reaches the dashboard: any
// other text a Job sends is shown as MsgFailed, so nothing the clone or
// graphify wrote is ever stored.
package codegraph

import "errors"

// ErrorBody is what a build posts when it has no graph.
type ErrorBody struct {
	Error string `json:"error"`
}

// The messages a build may post, and the ones pergent.sh records itself.
const (
	// The clone's, from scan.sh, and the post command's when the clone
	// wrote nothing.
	MsgCloneFetch   = "could not fetch the default branch"
	MsgCloneMoved   = "the default branch moved during the scan; scan again"
	MsgCloneUnknown = "the clone did not finish; see the log of the Job's clone container"
	// The graphify container's (kube's graphify script).
	MsgGraphify = "graphify couldn't read the repository"
	MsgConvert  = "pergent couldn't convert graphify's output"
	// The post command's.
	MsgNoGraph  = "the build ended without a graph; see the log of the Job's graphify container"
	MsgTooLarge = "the graph is over the 64 MiB limit"
	// pergent.sh's own.
	MsgFailed    = "the graph build failed; see the log of its Job"
	MsgUnusable  = "the graph build sent a graph pergent.sh couldn't use"
	MsgWrongHead = "the graph build sent a graph of another commit"
	MsgTimedOut  = "the graph build took longer than 15 minutes or ended without a result"
	MsgStart     = "couldn't start the graph build Job; see the pergent.sh log"
	MsgTarget    = "couldn't read the repository's default branch or clone token; see the pergent.sh log"
	MsgStore     = "couldn't store the graph; see the pergent.sh log"
	MsgPaused    = "the repository was paused before the build started; it builds again after it is switched on"
)

// shown maps each message a Job may send to what the dashboard shows.
var shown = map[string]string{
	MsgCloneFetch:   MsgCloneFetch,
	MsgCloneMoved:   "the default branch moved during the clone; the next check builds again",
	MsgCloneUnknown: MsgCloneUnknown,
	MsgGraphify:     MsgGraphify,
	MsgConvert:      MsgConvert,
	MsgNoGraph:      MsgNoGraph,
	MsgTooLarge:     MsgTooLarge,
}

// Shown is the dashboard's text for a message a build Job sent: its own
// wording for a known one, MsgFailed for anything else.
func Shown(msg string) string {
	if s, ok := shown[msg]; ok {
		return s
	}
	return MsgFailed
}

// Outcome is what became of a build's post.
type Outcome int

const (
	// Stored: the post was the build's and is recorded, as a graph or a
	// failure.
	Stored Outcome = iota + 1
	// Unauthorized: the token is not a running build's; nothing changed.
	Unauthorized
)

// ErrNoGraph is the answer for a review with no graph to download.
var ErrNoGraph = errors.New("no graph for this run")

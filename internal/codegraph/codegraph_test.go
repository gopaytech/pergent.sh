package codegraph

import "testing"

// A known message is shown in pergent.sh's words; any other text, however
// close, is the generic one, so a Job can't put its own text on the page.
func TestShown(t *testing.T) {
	for msg, want := range map[string]string{
		MsgGraphify:            MsgGraphify,
		MsgCloneMoved:          "the default branch moved during the clone; the next check builds again",
		MsgTooLarge:            MsgTooLarge,
		"":                     MsgFailed,
		MsgGraphify + " ":      MsgFailed,
		"<script>x</script>":   MsgFailed,
		"token ghs_0123456789": MsgFailed,
	} {
		if got := Shown(msg); got != want {
			t.Errorf("Shown(%q) = %q; want %q", msg, got, want)
		}
	}
}

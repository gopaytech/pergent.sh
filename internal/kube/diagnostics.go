package kube

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// CloneLogKeepBytes is how much of the checkout container's log a failed
	// review keeps (its tail).
	CloneLogKeepBytes = 64 * 1024
	// podStateEvents is how many of the newest events the pod state lists.
	podStateEvents = 10
)

// checkoutEnv is the value of an env var of the Job's checkout container, or "".
func (l *Launcher) checkoutEnv(ctx context.Context, jobName, name string) (string, error) {
	job, err := l.Client.BatchV1().Jobs(l.Namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get job %s: %w", jobName, err)
	}
	for _, c := range job.Spec.Template.Spec.InitContainers {
		if c.Name != containerCheckout {
			continue
		}
		for _, e := range c.Env {
			if e.Name == name {
				return e.Value, nil
			}
		}
	}
	return "", nil
}

// LinkedClones reads the linked repositories a Job clones from the checkout
// container's LINKED_CLONES env, in the order they were given. A Job without
// any has none.
func (l *Launcher) LinkedClones(ctx context.Context, jobName string) ([]LinkedClone, error) {
	value, err := l.checkoutEnv(ctx, jobName, "LINKED_CLONES")
	if err != nil {
		return nil, err
	}
	var out []LinkedClone
	for _, line := range strings.Split(value, "\n") {
		if dir, url, ok := strings.Cut(line, " "); ok && dir != "" && url != "" {
			out = append(out, LinkedClone{Dir: dir, CloneURL: url})
		}
	}
	return out, nil
}

// CheckoutRepoURL is the URL of the repository under review (the checkout
// container's REPO_URL), the one URL MaskLinked leaves in a clone log.
func (l *Launcher) CheckoutRepoURL(ctx context.Context, jobName string) (string, error) {
	return l.checkoutEnv(ctx, jobName, "REPO_URL")
}

// otherRepository stands in for a repository MaskLinked can't name.
const otherRepository = "<another repository>"

var urlInText = regexp.MustCompile("(?i)https?://[^\\s'\"<>`]+")

// MaskLinked makes a clone log safe to show to someone who may not know the
// linked repositories, failing closed: no linked repository's name is left in
// any form. A linked repository's URL (with or without ".git" or credentials,
// in any case) and its bare path ("group/name") become its directory name
// ("linked-1"). Every other URL but repoURL's becomes otherRepository, which
// covers a redirect to a repository's new place, and so does the target of a
// "was moved to" line. The rest of the text stays. With no linked repositories
// there is nothing to hide, since the only repository a clone log can name is
// the one under review (or its new place, which a viewer may see), so the text
// is returned as it is.
func MaskLinked(text string, clones []LinkedClone, repoURL string) string {
	if text == "" || len(clones) == 0 {
		return text
	}
	type target struct{ rest, dir string }
	var targets []target
	for _, c := range clones {
		if rest := hostPath(c.CloneURL); rest != "" && c.Dir != "" {
			targets = append(targets, target{rest, c.Dir})
		}
	}
	mainRest := hostPath(repoURL)
	if mainRest != "" {
		targets = append(targets, target{mainRest, ""})
	}
	sort.SliceStable(targets, func(i, j int) bool { return len(targets[i].rest) > len(targets[j].rest) })

	text = maskMoved(text)
	text = urlInText.ReplaceAllStringFunc(text, func(u string) string {
		tail := ""
		for len(u) > 0 && strings.ContainsRune(".,;:)]", rune(u[len(u)-1])) {
			u, tail = u[:len(u)-1], u[len(u)-1:]+tail
		}
		hp := afterScheme(u)
		lower := asciiLower(hp)
		for _, t := range targets {
			after, ok := strings.CutPrefix(lower, t.rest)
			if !ok {
				continue
			}
			// "g/other" is not the repository "g/other-two" or "g/other.github"
			skip := 0
			switch {
			case after == "" || after[0] == '/' || !isNameByte(after[0]) && after[0] != '.':
			case strings.HasPrefix(after, ".git") && (len(after) == 4 || !isNameByte(after[4]) && after[4] != '.'):
				skip = 4
			default:
				continue
			}
			if t.dir == "" {
				return u + tail
			}
			return t.dir + hp[len(t.rest)+skip:] + tail
		}
		return otherRepository + tail
	})

	// the bare path of each linked repository ("group/name")
	var alts []string
	names := map[string]string{}
	for _, t := range targets {
		if t.dir == "" {
			continue
		}
		if _, path, ok := strings.Cut(t.rest, "/"); ok && path != "" {
			names[path] = t.dir
			alts = append(alts, regexp.QuoteMeta(path))
		}
	}
	if len(alts) == 0 {
		return text
	}
	re := regexp.MustCompile("(?i)(?:" + strings.Join(alts, "|") + ")")
	var out strings.Builder
	last := 0
	for _, m := range re.FindAllStringIndex(text, -1) {
		// "g/other" is not the repository "g/other-two"
		if m[1] < len(text) && isNameByte(text[m[1]]) {
			continue
		}
		out.WriteString(text[last:m[0]])
		out.WriteString(names[asciiLower(text[m[0]:m[1]])])
		last = m[1]
	}
	out.WriteString(text[last:])
	return out.String()
}

// afterScheme is a URL without its scheme and credentials.
func afterScheme(u string) string {
	_, rest, ok := strings.Cut(u, "://")
	if !ok {
		return ""
	}
	if authority, path, ok := strings.Cut(rest, "/"); ok {
		if at := strings.LastIndex(authority, "@"); at >= 0 {
			authority = authority[at+1:]
		}
		return authority + "/" + path
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		return rest[at+1:]
	}
	return rest
}

// hostPath is a URL as lower-case "host/path", without scheme, credentials,
// trailing slash or ".git".
func hostPath(u string) string {
	return strings.TrimSuffix(strings.TrimRight(asciiLower(afterScheme(u)), "/"), ".git")
}

// asciiLower lower-cases ASCII letters only, so byte offsets stay the same.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// isNameByte reports whether b can continue a repository name.
func isNameByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-' || b == '_'
}

// maskMoved hides where a repository was moved to: git hosts say so on a
// "was moved to" line, with the target after it or on the next line.
func maskMoved(text string) string {
	lines := strings.Split(text, "\n")
	next := false
	for i, line := range lines {
		if next && strings.TrimSpace(line) != "" {
			next = false
			body := strings.TrimLeft(strings.TrimPrefix(strings.TrimLeft(line, " \t"), "remote:"), " \t")
			lines[i] = line[:len(line)-len(body)] + otherRepository
			continue
		}
		at := strings.Index(asciiLower(line), "moved to")
		if at < 0 {
			continue
		}
		head, rest := line[:at+len("moved to")], strings.TrimSpace(line[at+len("moved to"):])
		switch rest {
		case "", ":":
			next = true
		default:
			lines[i] = head + " " + otherRepository
		}
	}
	return strings.Join(lines, "\n")
}

// PodState describes the newest pod of a Job as text for an admin: its phase,
// its PodScheduled condition, each container's state, then the newest events
// of the pod and of the Job, oldest first. A Job with no pod has only its Job
// events. Events that can't be read are left out; eventsForbidden says the
// cluster refused to list them, so the text may lack them.
func (l *Launcher) PodState(ctx context.Context, jobName string) (text string, eventsForbidden bool, err error) {
	pods, err := l.jobPods(ctx, jobName)
	if err != nil {
		return "", false, err
	}
	var b strings.Builder
	var events []corev1.Event
	if n := len(pods); n > 0 {
		pod := &pods[n-1]
		writePodLines(&b, pod)
		// events that can't be read are left out, as forbidden ones are
		evs, forbidden, _ := l.ObjectEvents(ctx, "Pod", pod.Name)
		events, eventsForbidden = evs, forbidden
	}
	evs, forbidden, _ := l.ObjectEvents(ctx, "Job", jobName)
	events, eventsForbidden = append(events, evs...), eventsForbidden || forbidden

	sort.SliceStable(events, func(i, j int) bool { return eventTime(events[i]).Before(eventTime(events[j])) })
	if len(events) > podStateEvents {
		events = events[len(events)-podStateEvents:]
	}
	if len(events) > 0 {
		b.WriteString("Events, oldest first:\n")
	}
	for _, e := range events {
		count := max(e.Count, 1)
		fmt.Fprintf(&b, "%s %s %s (x%d): %s\n", eventTime(e).UTC().Format(time.RFC3339), e.Type, e.Reason, count, oneLine(e.Message))
	}
	return b.String(), eventsForbidden, nil
}

func writePodLines(b *strings.Builder, pod *corev1.Pod) {
	fmt.Fprintf(b, "Pod %s: phase %s\n", pod.Name, pod.Status.Phase)
	for _, c := range pod.Status.Conditions {
		if c.Type != corev1.PodScheduled {
			continue
		}
		fmt.Fprintf(b, "PodScheduled=%s", c.Status)
		if c.Reason != "" {
			b.WriteString(": " + c.Reason)
		}
		if c.Message != "" {
			b.WriteString(": " + oneLine(c.Message))
		}
		b.WriteString("\n")
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		writeContainerLine(b, cs, " (init)")
	}
	for _, cs := range pod.Status.ContainerStatuses {
		writeContainerLine(b, cs, "")
	}
}

func writeContainerLine(b *strings.Builder, cs corev1.ContainerStatus, kind string) {
	switch s := cs.State; {
	case s.Waiting != nil:
		fmt.Fprintf(b, "%s%s: waiting", cs.Name, kind)
		if s.Waiting.Reason != "" {
			b.WriteString(", reason " + s.Waiting.Reason)
		}
	case s.Terminated != nil:
		fmt.Fprintf(b, "%s%s: terminated", cs.Name, kind)
		if s.Terminated.Reason != "" {
			b.WriteString(", reason " + s.Terminated.Reason)
		}
		fmt.Fprintf(b, ", exit code %d", s.Terminated.ExitCode)
	case s.Running != nil:
		fmt.Fprintf(b, "%s%s: running", cs.Name, kind)
	default:
		return
	}
	b.WriteString("\n")
}

// oneLine keeps an event or scheduler message on one line, so the text stays
// one fact per line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

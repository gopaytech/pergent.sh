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

// LinkedClones reads the linked repositories a Job clones from the checkout
// container's LINKED_CLONES env, in the order they were given. A Job without
// any has none.
func (l *Launcher) LinkedClones(ctx context.Context, jobName string) ([]LinkedClone, error) {
	job, err := l.Client.BatchV1().Jobs(l.Namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get job %s: %w", jobName, err)
	}
	var out []LinkedClone
	for _, c := range job.Spec.Template.Spec.InitContainers {
		if c.Name != containerCheckout {
			continue
		}
		for _, e := range c.Env {
			if e.Name != "LINKED_CLONES" {
				continue
			}
			for _, line := range strings.Split(e.Value, "\n") {
				if dir, url, ok := strings.Cut(line, " "); ok && dir != "" && url != "" {
					out = append(out, LinkedClone{Dir: dir, CloneURL: url})
				}
			}
		}
	}
	return out, nil
}

// MaskLinked replaces each linked repository's URL in text by its directory
// name ("linked-1"), with or without the ".git" suffix and with credentials in
// front of the host, and keeps the rest. A URL that is only the start of a
// longer repository name is left alone.
func MaskLinked(text string, clones []LinkedClone) string {
	if text == "" || len(clones) == 0 {
		return text
	}
	sorted := append([]LinkedClone(nil), clones...)
	sort.SliceStable(sorted, func(i, j int) bool { return len(sorted[i].CloneURL) > len(sorted[j].CloneURL) })
	names := map[string]string{}
	var alts []string
	for _, c := range sorted {
		if c.CloneURL == "" || c.Dir == "" {
			continue
		}
		rest := strings.TrimSuffix(c.CloneURL, ".git")
		prefix := ""
		if r, ok := strings.CutPrefix(rest, "https://"); ok {
			prefix, rest = `https://(?:[^/@\s]+@)?`, r
		}
		names[rest] = c.Dir
		alts = append(alts, prefix+regexp.QuoteMeta(rest))
	}
	if len(alts) == 0 {
		return text
	}
	re := regexp.MustCompile("(?:" + strings.Join(alts, "|") + ")(?:\\.git)?")
	var out strings.Builder
	last := 0
	for _, m := range re.FindAllStringIndex(text, -1) {
		// "other" is not the repository "other-two"
		if m[1] < len(text) && isNameByte(text[m[1]]) {
			continue
		}
		out.WriteString(text[last:m[0]])
		out.WriteString(dirOf(text[m[0]:m[1]], names))
		last = m[1]
	}
	out.WriteString(text[last:])
	return out.String()
}

// isNameByte reports whether b can continue a repository name.
func isNameByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-' || b == '_'
}

// dirOf is the directory name of the URL a match holds: the match without its
// scheme, credentials and ".git" is a key of names.
func dirOf(match string, names map[string]string) string {
	s := strings.TrimSuffix(match, ".git")
	if r, ok := strings.CutPrefix(s, "https://"); ok {
		if at := strings.LastIndex(r, "@"); at >= 0 && !strings.Contains(r[:at], "/") {
			r = r[at+1:]
		}
		s = r
	}
	if d, ok := names[s]; ok {
		return d
	}
	return match
}

// PodState describes the newest pod of a Job as text for an admin: its phase,
// its PodScheduled condition, each container's state, then the newest events
// of the pod and of the Job, oldest first. A Job with no pod has only its Job
// events. eventsForbidden says the cluster refused to list events, so the text
// may lack them.
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
		evs, forbidden, err := l.ObjectEvents(ctx, "Pod", pod.Name)
		if err != nil {
			return "", false, fmt.Errorf("list events of pod %s: %w", pod.Name, err)
		}
		events, eventsForbidden = evs, forbidden
	}
	evs, forbidden, err := l.ObjectEvents(ctx, "Job", jobName)
	if err != nil {
		return "", false, fmt.Errorf("list events of job %s: %w", jobName, err)
	}
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

package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"
)

// apiRequest is a request for a review of subject under tenant by client.
func apiRequest(clientID int64, tenant, subject string) NewAPIReview {
	return NewAPIReview{ClientID: clientID, Tenant: tenant, Subject: subject, Title: "Route change", KeyName: "ci",
		BaseSHA: "base-" + subject, HeadSHA: "head-" + subject, InputsSealed: []byte("sealed-" + subject),
		FileCount: 2, FileBytes: 300}
}

func enqueueAPI(t *testing.T, s *Store, n NewAPIReview) string {
	t.Helper()
	id, created, err := s.EnqueueAPIReview(ctx, n)
	if err != nil || !created {
		t.Fatalf("EnqueueAPIReview(%s/%s): created=%v err=%v", n.Tenant, n.Subject, created, err)
	}
	return id
}

func apiReview(t *testing.T, s *Store, id string) APIReview {
	t.Helper()
	r, found, err := s.APIReviewByID(ctx, id)
	if err != nil || !found {
		t.Fatalf("APIReviewByID(%s): found=%v err=%v", id, found, err)
	}
	return r
}

func apiState(t *testing.T, s *Store, id string) RunState {
	t.Helper()
	return apiReview(t, s, id).Run.State
}

func TestEnqueueAPIReviewShape(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "fastgateway")
	id := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:1"))

	r := getRun(t, s, id)
	if r.Kind != KindAPI || r.APIClientID == nil || *r.APIClientID != c.ID {
		t.Errorf("kind = %q, client = %v; want api, %d", r.Kind, r.APIClientID, c.ID)
	}
	if r.MR.Instance != "" || r.MR.ProjectID != 0 || r.MR.IID <= 0 {
		t.Errorf("MR = %+v; want instance '', project 0 and a number of its own", r.MR)
	}
	if r.State != Queued || !r.QuotaChecked || r.Trigger != TriggerPush {
		t.Errorf("state = %s, quota checked = %v, trigger = %q", r.State, r.QuotaChecked, r.Trigger)
	}
	if r.BaseSHA != "base-route:1" || r.HeadSHA != "head-route:1" {
		t.Errorf("base, head = %q, %q", r.BaseSHA, r.HeadSHA)
	}

	got, found, err := s.APIReview(ctx, c.ID, id)
	if err != nil || !found {
		t.Fatalf("APIReview: found=%v err=%v", found, err)
	}
	if got.Run.ID != id || got.ClientID != c.ID || got.ClientName != "fastgateway" {
		t.Errorf("review = %+v", got)
	}
	if got.Tenant != "acme" || got.Subject != "route:1" || got.Title != "Route change" || got.KeyName != "ci" {
		t.Errorf("tenant, subject, title, key = %q, %q, %q, %q", got.Tenant, got.Subject, got.Title, got.KeyName)
	}
	if !got.HasInputs || got.Response != nil || !got.FinishedAt.IsZero() || got.CreatedAt.IsZero() {
		t.Errorf("inputs = %v, response = %q, finished = %v, created = %v", got.HasInputs, got.Response, got.FinishedAt, got.CreatedAt)
	}

	// Each API review gets a number of its own, so the live-run index never
	// joins two of them.
	other := getRun(t, s, enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:2")))
	if other.MR.IID == r.MR.IID {
		t.Errorf("two API reviews share the number %d", r.MR.IID)
	}

	// A merge request's run is of kind mr, with no client.
	if mr := getRun(t, s, enqueue(t, s, mr1, "aaa")); mr.Kind != KindMR || mr.APIClientID != nil {
		t.Errorf("MR run: kind = %q, client = %v", mr.Kind, mr.APIClientID)
	}
	// An API run must have no repository and a client.
	if _, err := s.pool.Exec(ctx, `INSERT INTO runs (instance, project_id, mr_iid, head_sha, kind, api_client_id)
		VALUES ('gh', 0, 99, 'h', 'api', $1)`, c.ID); err == nil {
		t.Error("an API run with an instance was stored")
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO runs (instance, project_id, mr_iid, head_sha, kind)
		VALUES ('', 0, 98, 'h', 'api')`); err == nil {
		t.Error("an API run with no client was stored")
	}
}

func TestEnqueueAPIReviewSupersedesSubject(t *testing.T) {
	s := newStore(t)
	a := newAPIClient(t, s, "client-a")
	b := newAPIClient(t, s, "client-b")

	first := enqueueAPI(t, s, apiRequest(a.ID, "acme", "route:1"))
	second := enqueueAPI(t, s, apiRequest(a.ID, "acme", "route:1"))
	if got := apiState(t, s, first); got != Superseded {
		t.Errorf("first state = %s, want superseded", got)
	}
	if got := apiState(t, s, second); got != Queued {
		t.Errorf("second state = %s, want queued", got)
	}

	// The empty tenant is a tenant of its own, and so is another client.
	noTenant := enqueueAPI(t, s, apiRequest(a.ID, "", "route:1"))
	otherClient := enqueueAPI(t, s, apiRequest(b.ID, "acme", "route:1"))
	for name, id := range map[string]string{"second": second, "no tenant": noTenant, "other client": otherClient} {
		if got := apiState(t, s, id); got != Queued {
			t.Errorf("%s state = %s, want queued", name, got)
		}
	}

	// A running review, Job and all, is superseded too.
	setRun(t, s, second, `state = 'running', job_name = 'job-second'`)
	third := enqueueAPI(t, s, apiRequest(a.ID, "acme", "route:1"))
	if got := apiState(t, s, second); got != Superseded {
		t.Errorf("running review state = %s, want superseded", got)
	}
	if got := apiState(t, s, third); got != Queued {
		t.Errorf("third state = %s, want queued", got)
	}
}

func TestEnqueueAPIReviewKeepsReported(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "fastgateway")
	first := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:1"))
	setRun(t, s, first, `state = 'reported', job_name = 'job-first', result = '{"status":"completed"}'::jsonb`)

	second := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:1"))
	if got := apiState(t, s, first); got != Reported {
		t.Errorf("reported review state = %s, want reported: it finishes", got)
	}
	if got := apiState(t, s, second); got != Queued {
		t.Errorf("new review state = %s, want queued", got)
	}
}

func TestEnqueueAPIReviewSameContentTwoSubjects(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "fastgateway")
	n1, n2 := apiRequest(c.ID, "acme", "route:1"), apiRequest(c.ID, "acme", "route:2")
	n1.BaseSHA, n1.HeadSHA = "b0", "h0"
	n2.BaseSHA, n2.HeadSHA = "b0", "h0"
	one := enqueueAPI(t, s, n1)
	two := enqueueAPI(t, s, n2)
	if one == two {
		t.Fatal("two subjects got one review")
	}
	for _, id := range []string{one, two} {
		if got := apiState(t, s, id); got != Queued {
			t.Errorf("%s state = %s, want queued", id, got)
		}
	}
}

func TestEnqueueAPIReviewIdempotent(t *testing.T) {
	s := newStore(t)
	a := newAPIClient(t, s, "client-a")
	b := newAPIClient(t, s, "client-b")
	n := apiRequest(a.ID, "acme", "route:1")
	n.IdempotencyKey, n.RequestHash = "retry-1", hash("request-1")

	id := enqueueAPI(t, s, n)
	again, created, err := s.EnqueueAPIReview(ctx, n)
	if err != nil || created || again != id {
		t.Fatalf("retry = %s, created=%v, err=%v; want %s, false", again, created, err, id)
	}
	if got := apiState(t, s, id); got != Queued {
		t.Errorf("state after the retry = %s, want queued: nothing is superseded", got)
	}

	setRun(t, s, id, `state = 'published', result = '{"status":"completed"}'::jsonb`)
	again, created, err = s.EnqueueAPIReview(ctx, n)
	if err != nil || created || again != id {
		t.Errorf("retry after publish = %s, created=%v, err=%v; want %s, false", again, created, err, id)
	}
	if got := apiState(t, s, id); got != Published {
		t.Errorf("state after the late retry = %s, want published", got)
	}

	other := n
	other.RequestHash = hash("request-2")
	if _, _, err := s.EnqueueAPIReview(ctx, other); !errors.Is(err, ErrIdempotencyMismatch) {
		t.Errorf("same key, other request: err = %v, want ErrIdempotencyMismatch", err)
	}

	otherClient := n
	otherClient.ClientID = b.ID
	theirs, created, err := s.EnqueueAPIReview(ctx, otherClient)
	if err != nil || !created || theirs == id {
		t.Errorf("same key, other client = %s, created=%v, err=%v; want a new review", theirs, created, err)
	}

	// No key: every request is a review of its own.
	plain := apiRequest(a.ID, "acme", "route:2")
	p1 := enqueueAPI(t, s, plain)
	p2 := enqueueAPI(t, s, plain)
	if p1 == p2 {
		t.Error("two requests with no key got one review")
	}
	list, err := s.APIReviews(ctx, APIReviewFilter{ClientID: a.ID})
	if err != nil || len(list) != 3 {
		t.Errorf("client a has %d reviews (err %v), want 3", len(list), err)
	}
}

func TestAPIReviewOtherClientNotFound(t *testing.T) {
	s := newStore(t)
	a := newAPIClient(t, s, "client-a")
	b := newAPIClient(t, s, "client-b")
	theirs := enqueueAPI(t, s, apiRequest(b.ID, "acme", "route:1"))

	if _, found, err := s.APIReview(ctx, a.ID, theirs); err != nil || found {
		t.Errorf("client a reading client b's review: found=%v err=%v", found, err)
	}
	if r, found, err := s.APIReview(ctx, b.ID, theirs); err != nil || !found || r.ClientName != "client-b" {
		t.Errorf("client b reading its review = %+v, %v, %v", r, found, err)
	}
	if r := apiReview(t, s, theirs); r.ClientID != b.ID {
		t.Errorf("APIReviewByID client = %d, want %d", r.ClientID, b.ID)
	}
	for _, id := range []string{"not-a-uuid", "00000000-0000-0000-0000-000000000000"} {
		if _, found, err := s.APIReviewByID(ctx, id); err != nil || found {
			t.Errorf("APIReviewByID(%q): found=%v err=%v", id, found, err)
		}
	}
	// A merge request's run is no API review.
	if _, found, err := s.APIReviewByID(ctx, enqueue(t, s, mr1, "aaa")); err != nil || found {
		t.Errorf("APIReviewByID(MR run): found=%v err=%v", found, err)
	}
}

func TestDropAPIInputs(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "fastgateway")
	id := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:1"))

	in, found, err := s.APIReviewInputs(ctx, id)
	if err != nil || !found || !bytes.Equal(in, []byte("sealed-route:1")) {
		t.Fatalf("APIReviewInputs = %q, %v, %v", in, found, err)
	}
	if err := s.DropAPIInputs(ctx, id); err != nil {
		t.Fatalf("DropAPIInputs: %v", err)
	}
	if _, found, err := s.APIReviewInputs(ctx, id); err != nil || found {
		t.Errorf("inputs after the drop: found=%v err=%v", found, err)
	}
	if apiReview(t, s, id).HasInputs {
		t.Error("HasInputs after the drop")
	}
	if err := s.DropAPIInputs(ctx, id); err != nil {
		t.Errorf("a second drop: %v", err)
	}

	superseded := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:2"))
	queued := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:2"))
	withJob := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:3"))
	setRun(t, s, withJob, `state = 'running', job_name = 'job-3'`)
	claimed := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:4"))
	setRun(t, s, claimed, `state = 'running'`) // claimed, no Job yet
	failedEarly := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:5"))
	setRun(t, s, failedEarly, `state = 'reported', failure_reason = 'review failed'`)

	n, err := s.DropSpentAPIInputs(ctx)
	if err != nil || n != 3 {
		t.Errorf("DropSpentAPIInputs = %d, %v; want 3", n, err)
	}
	for name, tc := range map[string]struct {
		id   string
		kept bool
	}{
		"superseded": {superseded, false}, "with a job": {withJob, false}, "failed before a job": {failedEarly, false},
		"queued": {queued, true}, "claimed": {claimed, true},
	} {
		if _, found, err := s.APIReviewInputs(ctx, tc.id); err != nil || found != tc.kept {
			t.Errorf("%s: inputs kept = %v (err %v), want %v", name, found, err, tc.kept)
		}
	}
	if n, err := s.DropSpentAPIInputs(ctx); err != nil || n != 0 {
		t.Errorf("a second DropSpentAPIInputs = %d, %v; want 0", n, err)
	}
}

func TestFinishAPIReview(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "fastgateway")
	id := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:1"))
	setRun(t, s, id, `state = 'reported', job_name = 'job-1', result = '{"status":"completed"}'::jsonb`)

	response := []byte(`{"id":"` + id + `","status":"completed","verdict":"nothing_reviewed"}`)
	dec := RunDecision{Outcome: OutcomeNothingReviewed, Reason: "no skill reviewed a file"}
	if err := s.FinishAPIReview(ctx, id, response, dec); err != nil {
		t.Fatalf("FinishAPIReview: %v", err)
	}
	r := apiReview(t, s, id)
	if r.Run.State != Published || r.Run.Outcome != OutcomeNothingReviewed || r.Run.OutcomeReason != dec.Reason {
		t.Errorf("state, outcome, reason = %s, %q, %q", r.Run.State, r.Run.Outcome, r.Run.OutcomeReason)
	}
	var got, want any
	if err := json.Unmarshal(r.Response, &got); err != nil {
		t.Fatalf("stored response %q: %v", r.Response, err)
	}
	if err := json.Unmarshal(response, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("response = %s, want %s", r.Response, response)
	}
	if r.FinishedAt.IsZero() {
		t.Error("FinishedAt is not set")
	}
	if err := s.FinishAPIReview(ctx, id, response, dec); err == nil {
		t.Error("a published review was finished again")
	}

	queued := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:2"))
	if err := s.FinishAPIReview(ctx, queued, response, dec); err == nil {
		t.Error("a queued review was finished")
	}
	if r := apiReview(t, s, queued); r.Run.State != Queued || r.Response != nil || !r.FinishedAt.IsZero() {
		t.Errorf("queued review after a refused finish = %s, %q, %v", r.Run.State, r.Response, r.FinishedAt)
	}

	bad := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:3"))
	setRun(t, s, bad, `state = 'reported', result = '{}'::jsonb`)
	if err := s.FinishAPIReview(ctx, bad, []byte("{"), dec); !errors.Is(err, ErrInvalidResult) {
		t.Errorf("a response that is not JSON: err = %v, want ErrInvalidResult", err)
	}
	if got := apiState(t, s, bad); got != Reported {
		t.Errorf("state after a refused response = %s, want reported", got)
	}
}

func TestAPIReviewsFilter(t *testing.T) {
	s := newStore(t)
	a := newAPIClient(t, s, "client-a")
	b := newAPIClient(t, s, "client-b")

	queued := enqueueAPI(t, s, apiRequest(a.ID, "acme", "s1"))
	claimed := enqueueAPI(t, s, apiRequest(a.ID, "acme", "s2"))
	setRun(t, s, claimed, `state = 'running'`)
	running := enqueueAPI(t, s, apiRequest(a.ID, "acme", "s3"))
	setRun(t, s, running, `state = 'running', job_name = 'job-s3'`)
	reported := enqueueAPI(t, s, apiRequest(a.ID, "beta", "s1"))
	setRun(t, s, reported, `state = 'reported', job_name = 'job-r', result = '{"status":"completed"}'::jsonb`)
	completed := enqueueAPI(t, s, apiRequest(a.ID, "beta", "s4"))
	setRun(t, s, completed, `state = 'published', result = '{"status":"completed"}'::jsonb`)
	failed := enqueueAPI(t, s, apiRequest(a.ID, "beta", "s5"))
	setRun(t, s, failed, `state = 'published', result = '{"status":"failed"}'::jsonb`)
	failedEarly := enqueueAPI(t, s, apiRequest(a.ID, "", "s6"))
	setRun(t, s, failedEarly, `state = 'published', failure_reason = 'review failed'`)
	gaveUp := enqueueAPI(t, s, apiRequest(a.ID, "", "s7"))
	setRun(t, s, gaveUp, `state = 'publish_failed', result = '{"status":"completed"}'::jsonb`)
	superseded := enqueueAPI(t, s, apiRequest(a.ID, "", "s8"))
	newer := enqueueAPI(t, s, apiRequest(a.ID, "", "s8"))
	enqueueAPI(t, s, apiRequest(b.ID, "acme", "s1")) // another client's
	enqueue(t, s, mr1, "aaa")                        // a merge request's run

	ids := func(f APIReviewFilter) []string {
		t.Helper()
		list, err := s.APIReviews(ctx, f)
		if err != nil {
			t.Fatalf("APIReviews(%+v): %v", f, err)
		}
		out := make([]string, 0, len(list))
		for _, r := range list {
			if r.ClientID != a.ID {
				t.Errorf("APIReviews(%+v) returned client %d's review", f, r.ClientID)
			}
			out = append(out, r.Run.ID)
		}
		slices.Sort(out)
		return out
	}
	set := func(id ...string) []string { slices.Sort(id); return id }

	for name, tc := range map[string]struct {
		f    APIReviewFilter
		want []string
	}{
		"all":         {APIReviewFilter{ClientID: a.ID}, set(queued, claimed, running, reported, completed, failed, failedEarly, gaveUp, superseded, newer)},
		"tenant acme": {APIReviewFilter{ClientID: a.ID, Tenant: "acme"}, set(queued, claimed, running)},
		"subject s1":  {APIReviewFilter{ClientID: a.ID, Subject: "s1"}, set(queued, reported)},
		"acme s1":     {APIReviewFilter{ClientID: a.ID, Tenant: "acme", Subject: "s1"}, set(queued)},
		"queued":      {APIReviewFilter{ClientID: a.ID, Status: "queued"}, set(queued, claimed, newer)},
		"running":     {APIReviewFilter{ClientID: a.ID, Status: "running"}, set(running, reported)},
		"completed":   {APIReviewFilter{ClientID: a.ID, Status: "completed"}, set(completed)},
		"failed":      {APIReviewFilter{ClientID: a.ID, Status: "failed"}, set(failed, failedEarly, gaveUp)},
		"superseded":  {APIReviewFilter{ClientID: a.ID, Status: "superseded"}, set(superseded)},
		"beta failed": {APIReviewFilter{ClientID: a.ID, Tenant: "beta", Status: "failed"}, set(failed)},
		"no client":   {APIReviewFilter{}, set()},
		"subject s8":  {APIReviewFilter{ClientID: a.ID, Subject: "s8"}, set(superseded, newer)},
	} {
		if got := ids(tc.f); !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
	if _, err := s.APIReviews(ctx, APIReviewFilter{ClientID: a.ID, Status: "published"}); err == nil {
		t.Error("an unknown status was accepted")
	}
}

func TestAPIReviewsPages(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "fastgateway")
	oldest := enqueueAPI(t, s, apiRequest(c.ID, "acme", "s1"))
	tied1 := enqueueAPI(t, s, apiRequest(c.ID, "acme", "s2"))
	tied2 := enqueueAPI(t, s, apiRequest(c.ID, "acme", "s3"))
	newest := enqueueAPI(t, s, apiRequest(c.ID, "acme", "s4"))
	at := map[string]time.Time{oldest: t0, tied1: t0.Add(time.Minute), tied2: t0.Add(time.Minute), newest: t0.Add(2 * time.Minute)}
	for id, when := range at {
		if _, err := s.pool.Exec(ctx, `UPDATE api_reviews SET created_at = $2 WHERE run_id = $1::uuid`, id, when); err != nil {
			t.Fatal(err)
		}
	}
	// Newest first; reviews made at the same time go by ID, highest first.
	tied := []string{tied1, tied2}
	slices.Sort(tied)
	want := []string{newest, tied[1], tied[0], oldest}

	all, err := s.APIReviews(ctx, APIReviewFilter{ClientID: c.ID})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range all {
		got = append(got, r.Run.ID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}

	got = nil
	var after *APIReviewCursor
	for page := 0; page < 10; page++ {
		list, err := s.APIReviews(ctx, APIReviewFilter{ClientID: c.ID, After: after, Limit: 1})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(list) == 0 {
			break
		}
		if len(list) != 1 {
			t.Fatalf("page %d has %d reviews, want 1", page, len(list))
		}
		r := list[0]
		got = append(got, r.Run.ID)
		after = &APIReviewCursor{CreatedAt: r.CreatedAt, ID: r.Run.ID}
	}
	if !slices.Equal(got, want) {
		t.Errorf("pages = %v, want %v", got, want)
	}

	if _, err := s.APIReviews(ctx, APIReviewFilter{ClientID: c.ID, After: &APIReviewCursor{CreatedAt: t0, ID: "nope"}}); err == nil {
		t.Error("a cursor with a malformed ID was accepted")
	}
}

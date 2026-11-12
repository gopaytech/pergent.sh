package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
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

func TestAPIReviewByKey(t *testing.T) {
	s := newStore(t)
	a := newAPIClient(t, s, "client-a")
	b := newAPIClient(t, s, "client-b")
	n := apiRequest(a.ID, "acme", "route:1")
	n.IdempotencyKey, n.RequestHash = "retry-1", hash("request-1")
	id := enqueueAPI(t, s, n)

	r, got, found, err := s.APIReviewByKey(ctx, a.ID, "retry-1")
	if err != nil || !found || r.Run.ID != id || !bytes.Equal(got, n.RequestHash) {
		t.Errorf("APIReviewByKey = %s, %x, %v, %v; want %s, %x, true", r.Run.ID, got, found, err, id, n.RequestHash)
	}
	// The key is the client's own: another client, an unused key and a review
	// asked for with no key are all not found.
	for name, c := range map[string]struct {
		client int64
		key    string
	}{"another client": {b.ID, "retry-1"}, "unused key": {a.ID, "retry-2"}, "no key": {a.ID, ""}} {
		if _, _, found, err := s.APIReviewByKey(ctx, c.client, c.key); err != nil || found {
			t.Errorf("%s: found=%v err=%v, want not found", name, found, err)
		}
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

// seedAPIUsage makes an API review of client that finished at the given time
// and records its skills, as seedUsage does for a merge request.
func seedAPIUsage(t *testing.T, s *Store, clientID int64, subject string, at time.Time, skills ...SkillUsage) string {
	t.Helper()
	return seedTenantUsage(t, s, clientID, "acme", subject, at, skills...)
}

// seedTenantUsage is seedAPIUsage for a review asked for under tenant.
func seedTenantUsage(t *testing.T, s *Store, clientID int64, tenant, subject string, at time.Time, skills ...SkillUsage) string {
	t.Helper()
	id := enqueueAPI(t, s, apiRequest(clientID, tenant, subject))
	setRun(t, s, id, `state = 'reported', created_at = $2::timestamptz, reported_at = $2::timestamptz, dispatched_at = $2::timestamptz,
		trigger = $3::text, model = 'm', result = '{"schema_version":1,"status":"completed","skills":[]}'::jsonb`, at, TriggerPush)
	if err := s.RecordUsage(ctx, id, skills); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestReviewDigestsLeaveOutAPIReviews(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "fastgateway")
	mr := dispatched(t, s, mr1, "aaa", "tok-aaa")
	api := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:1"))

	got, err := s.ReviewDigests(ctx, ReviewFilter{Visible: Visibility{All: true}, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != mr {
		ids := make([]string, len(got))
		for i, d := range got {
			ids[i] = d.ID
		}
		t.Errorf("digests = %v, want only the merge request's run %s (not the API review %s)", ids, mr, api)
	}
}

func TestUsageByRepositoryLeavesOutAPIReviews(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "fastgateway")
	seedUsage(t, s, usageMR(7, 1), usageAt(5, 10, 0), TriggerPush, false, "m", skillRow("security", "no_blocking_issues", 100, 10))
	seedAPIUsage(t, s, c.ID, "route:1", usageAt(5, 11, 0), skillRow("gateway", "no_blocking_issues", 5000, 500))

	got, err := s.UsageByRepository(ctx, usageSince)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ProjectID != 7 || got[0].Instance != mr1.Instance {
		t.Fatalf("UsageByRepository = %+v, want only project 7", got)
	}
	wantTokens(t, "project 7", got[0].Tokens, TokenSum{In: 100, Out: 10})

	var rows []UsageRow
	if err := s.UsageRows(ctx, usageSince, "", 0, 100, func(u UsageRow) error {
		rows = append(rows, u)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Skill != "security" {
		t.Errorf("UsageRows = %+v, want only the merge request's row", rows)
	}
}

// The admin overview counts API reviews too, under the empty repository.
func TestRunStatsCountAPIReviews(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "fastgateway")
	enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:1"))
	enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:2"))
	dispatched(t, s, mr1, "aaa", "tok-aaa")

	got, err := s.RunStats(ctx, Visibility{All: true}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n := got[PathKey{}].Reviews; n != 2 {
		t.Errorf("API reviews = %d, want 2 (stats %+v)", n, got)
	}
	if n := got[PathKey{Instance: mr1.Instance, Path: "acme/widgets"}].Reviews; n != 1 {
		t.Errorf("merge request reviews = %d, want 1 (stats %+v)", n, got)
	}
}

func TestMetricsTotalsLeaveOutAPIReviews(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "fastgateway")
	seedUsage(t, s, usageMR(7, 1), usageAt(5, 10, 0), TriggerPush, false, "m", skillRow("security", "no_blocking_issues", 100, 10))
	seedAPIUsage(t, s, c.ID, "route:1", usageAt(5, 11, 0), skillRow("gateway", "failed", 5000, 500))
	// One API review that failed before it left a result, and one a newer
	// request stopped: both are counted from runs, not run_usage.
	failed := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:2"))
	setRun(t, s, failed, `state = 'reported', dispatched_at = now(), failure_reason = 'the Job failed'`)
	stopped := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:3"))
	setRun(t, s, stopped, `dispatched_at = now()`)
	enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:3"))
	if apiState(t, s, stopped) != Superseded {
		t.Fatal("the first request was not superseded")
	}

	got, err := s.MetricsTotals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tokens) != 1 || len(got.Reviews) != 1 || len(got.SkillRuns) != 1 || len(got.Findings) != 1 {
		t.Errorf("usage totals = %d, %d, %d, %d rows, want the merge request's one each", len(got.Tokens), len(got.Reviews), len(got.SkillRuns), len(got.Findings))
	}
	var instances []string
	for _, r := range got.Tokens {
		instances = append(instances, r.Instance)
	}
	for _, r := range got.Reviews {
		instances = append(instances, r.Instance)
	}
	for _, r := range got.NoResult {
		instances = append(instances, r.Instance)
	}
	for _, r := range got.Unfinished {
		instances = append(instances, r.Instance)
	}
	for _, r := range got.SkillRuns {
		instances = append(instances, r.Instance)
	}
	for _, r := range got.Findings {
		instances = append(instances, r.Instance)
	}
	for _, r := range got.Dismissals {
		instances = append(instances, r.Instance)
	}
	if len(instances) == 0 {
		t.Fatal("no totals at all")
	}
	for _, in := range instances {
		if in == "" {
			t.Errorf("a total has the empty instance: %+v", got)
			break
		}
	}
	if len(got.NoResult) != 0 || len(got.Unfinished) != 0 {
		t.Errorf("no result = %+v, unfinished = %+v, want none: the merge request has none", got.NoResult, got.Unfinished)
	}
}

func TestDeleteAPIClientReturnsLiveJobs(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "fastgateway")
	other := newAPIClient(t, s, "other")

	live := seedAPIUsage(t, s, c.ID, "route:1", usageAt(5, 10, 0), skillRow("gateway", "no_blocking_issues", 10, 1))
	setRun(t, s, live, `state = 'running', job_name = 'pergent-review-abc'`)
	// Only a running review with a Job is named.
	done := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:2"))
	setRun(t, s, done, `state = 'reported', job_name = 'pergent-review-done'`)
	queued := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:3"))
	otherLive := enqueueAPI(t, s, apiRequest(other.ID, "acme", "route:1"))
	setRun(t, s, otherLive, `state = 'running', job_name = 'pergent-review-other'`)

	jobs, found, err := s.DeleteAPIClient(ctx, "fastgateway")
	if err != nil || !found {
		t.Fatalf("DeleteAPIClient = %v, %v, %v", jobs, found, err)
	}
	if !slices.Equal(jobs, []string{"pergent-review-abc"}) {
		t.Errorf("jobs = %v, want [pergent-review-abc]", jobs)
	}

	count := func(q string, id string) int {
		t.Helper()
		var n int
		if err := s.pool.QueryRow(ctx, q, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, id := range []string{live, done, queued} {
		if n := count(`SELECT count(*) FROM runs WHERE id = $1::uuid`, id); n != 0 {
			t.Errorf("run %s survived its client", id)
		}
		if n := count(`SELECT count(*) FROM api_reviews WHERE run_id = $1::uuid`, id); n != 0 {
			t.Errorf("api_reviews row of %s survived its client", id)
		}
		if n := count(`SELECT count(*) FROM run_usage WHERE run_id = $1::uuid`, id); n != 0 {
			t.Errorf("run_usage rows of %s survived its client", id)
		}
	}
	if n := count(`SELECT count(*) FROM runs WHERE id = $1::uuid`, otherLive); n != 1 {
		t.Error("another client's run was deleted")
	}

	// A client with one live review names its Job, and an unknown client is not found.
	if jobs, found, err := s.DeleteAPIClient(ctx, "other"); err != nil || !found || len(jobs) != 1 {
		t.Errorf("DeleteAPIClient(other) = %v, %v, %v", jobs, found, err)
	}
	if jobs, found, err := s.DeleteAPIClient(ctx, "gone"); err != nil || found || jobs != nil {
		t.Errorf("DeleteAPIClient(gone) = %v, %v, %v", jobs, found, err)
	}
}

// The merge and close sweep looks changes up on a forge, which an API review
// has none on, even once it is published with an outcome.
func TestChangesWithoutFatesLeaveOutAPIReviews(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "fastgateway")
	id := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:1"))
	setRun(t, s, id, `state = 'reported', job_name = 'job-1', result = '{"status":"completed"}'::jsonb`)
	dec := RunDecision{Outcome: OutcomeNothingReviewed, Reason: "no skill reviewed a file"}
	if err := s.FinishAPIReview(ctx, id, []byte(`{"status":"completed"}`), dec); err != nil {
		t.Fatalf("FinishAPIReview: %v", err)
	}
	decidedRun(t, s, mr1, "aaa", time.Now().Add(-time.Hour))

	got, err := s.ChangesWithoutFates(ctx, time.Now().Add(-24*time.Hour), 10)
	if err != nil || len(got) != 1 || got[0].MR != mr1 {
		t.Errorf("ChangesWithoutFates = %+v, %v; want only the merge request %+v", got, err, mr1)
	}
}

func TestAPIClientStats(t *testing.T) {
	s := newStore(t)
	a := newAPIClient(t, s, "client-a")
	b := newAPIClient(t, s, "client-b")
	idle := newAPIClient(t, s, "client-idle")
	newAPIKey(t, s, a.ID, "ci", "11111111")
	newAPIKey(t, s, a.ID, "ops", "22222222")
	newAPIKey(t, s, b.ID, "ci", "33333333")

	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	setCreated := func(id string, at time.Time) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, `UPDATE api_reviews SET created_at = $2 WHERE run_id = $1::uuid`, id, at); err != nil {
			t.Fatal(err)
		}
	}
	inMonth := since.Add(48 * time.Hour)

	ok := seedAPIUsage(t, s, a.ID, "s1", inMonth, SkillUsage{Skill: "security", Verdict: "no_blocking_issues", In: 100, Out: 20},
		SkillUsage{Skill: "style", Verdict: "no_blocking_issues", In: 30, Out: 5})
	setCreated(ok, inMonth)
	setRun(t, s, ok, `state = 'published'`)
	bad := seedAPIUsage(t, s, a.ID, "s2", inMonth, SkillUsage{Skill: "security", Verdict: "failed", In: 7, Out: 3})
	setCreated(bad, inMonth)
	setRun(t, s, bad, `state = 'published', result = '{"status":"failed"}'::jsonb`)
	queued := enqueueAPI(t, s, apiRequest(a.ID, "acme", "s3")) // no usage yet
	setCreated(queued, inMonth)
	old := seedAPIUsage(t, s, a.ID, "s4", since.Add(-time.Second), SkillUsage{Skill: "security", Verdict: "failed", In: 1000, Out: 1000})
	setCreated(old, since.Add(-time.Second))
	setRun(t, s, old, `state = 'published', result = '{"status":"failed"}'::jsonb`)
	other := seedAPIUsage(t, s, b.ID, "s1", inMonth, SkillUsage{Skill: "security", Verdict: "no_blocking_issues", In: 4, Out: 1})
	setCreated(other, inMonth)

	got, err := s.APIClientStats(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]APIClientStats{
		a.ID:    {Keys: 2, Reviews: 3, Failed: 1, Tokens: 165},
		b.ID:    {Keys: 1, Reviews: 1, Failed: 0, Tokens: 5},
		idle.ID: {},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("APIClientStats = %+v, want %+v", got, want)
	}
}

// The budget counts a month as the dashboard does: by when each review was
// asked for, the tokens of the client's reviews and no other's.
func TestAPITokensSince(t *testing.T) {
	s := newStore(t)
	a := newAPIClient(t, s, "client-a")
	b := newAPIClient(t, s, "client-b")
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	inMonth := since.Add(48 * time.Hour)
	setCreated := func(id string, at time.Time) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, `UPDATE api_reviews SET created_at = $2 WHERE run_id = $1::uuid`, id, at); err != nil {
			t.Fatal(err)
		}
	}

	if got, err := s.APITokensSince(ctx, a.ID, since); err != nil || got != 0 {
		t.Fatalf("a client with no review: %d, %v; want 0", got, err)
	}
	setCreated(seedAPIUsage(t, s, a.ID, "s1", inMonth, SkillUsage{Skill: "security", Verdict: "no_blocking_issues", In: 100, Out: 20},
		SkillUsage{Skill: "style", Verdict: "no_blocking_issues", In: 30, Out: 5}), inMonth)
	failed := seedAPIUsage(t, s, a.ID, "s2", inMonth, SkillUsage{Skill: "security", Verdict: "failed", In: 7, Out: 3})
	setCreated(failed, inMonth)
	setRun(t, s, failed, `state = 'published', result = '{"status":"failed"}'::jsonb`)
	// Asked for before the month, reported in it: last month's.
	setCreated(seedAPIUsage(t, s, a.ID, "s3", inMonth, SkillUsage{Skill: "security", Verdict: "failed", In: 1000, Out: 1000}),
		since.Add(-time.Second))
	setCreated(seedAPIUsage(t, s, b.ID, "s1", inMonth, SkillUsage{Skill: "security", Verdict: "no_blocking_issues", In: 4, Out: 1}), inMonth)

	got, err := s.APITokensSince(ctx, a.ID, since)
	if err != nil || got != 165 {
		t.Errorf("APITokensSince = %d, %v; want 165", got, err)
	}
	stats, err := s.APIClientStats(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if stats[a.ID].Tokens != got {
		t.Errorf("the dashboard shows %d tokens, the budget counts %d", stats[a.ID].Tokens, got)
	}
}

func TestLastAPIReview(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "gateway")
	other := newAPIClient(t, s, "other")
	published := func(clientID int64, tenant, subject, status string, at time.Time) string {
		t.Helper()
		id := enqueueAPI(t, s, apiRequest(clientID, tenant, subject))
		setRun(t, s, id, `state = 'published', result = jsonb_build_object('status', $2::text), created_at = $3::timestamptz`, status, at)
		return id
	}
	older := published(c.ID, "acme", "route:1", "completed", t0)
	newer := published(c.ID, "acme", "route:1", "completed", t0.Add(time.Hour))
	published(c.ID, "acme", "route:1", "error", t0.Add(2*time.Hour))
	// Newer, and of another tenant, subject or client: never the subject's.
	published(c.ID, "beta", "route:1", "completed", t0.Add(3*time.Hour))
	published(c.ID, "acme", "route:2", "completed", t0.Add(3*time.Hour))
	theirs := published(other.ID, "acme", "route:1", "completed", t0.Add(3*time.Hour))
	reported := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:1"))
	setRun(t, s, reported, `state = 'reported', result = '{"status":"completed"}'::jsonb, created_at = $2::timestamptz`, t0.Add(4*time.Hour))
	current := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:1"))

	got, found, err := s.LastAPIReview(ctx, c.ID, "acme", "route:1", current)
	if err != nil || !found || got.ID != newer {
		t.Fatalf("LastAPIReview = %s, %v, %v; want the newest completed published review %s", got.ID, found, err, newer)
	}
	if string(got.Result) == "" {
		t.Error("the review comes without its result")
	}
	// The run asking is never its own previous review.
	if got, found, err := s.LastAPIReview(ctx, c.ID, "acme", "route:1", newer); err != nil || !found || got.ID != older {
		t.Errorf("before %s = %s, %v, %v; want %s", newer, got.ID, found, err, older)
	}
	if got, found, err := s.LastAPIReview(ctx, other.ID, "acme", "route:1", current); err != nil || !found || got.ID != theirs {
		t.Errorf("the other client's = %s, %v, %v; want its own %s", got.ID, found, err, theirs)
	}
	if _, found, err := s.LastAPIReview(ctx, c.ID, "acme", "route:9", current); err != nil || found {
		t.Errorf("a subject with no review: found %v, %v", found, err)
	}
	if _, found, err := s.LastAPIReview(ctx, c.ID, "", "route:1", current); err != nil || found {
		t.Errorf("the empty tenant is a tenant of its own: found %v, %v", found, err)
	}
}

// setMaxInFlight sets the client's max_in_flight.
func setMaxInFlight(t *testing.T, s *Store, name string, n int) {
	t.Helper()
	if _, _, err := s.UpdateAPIClient(ctx, name, func(c *APIClient) { c.Limits.MaxInFlight = n }); err != nil {
		t.Fatalf("UpdateAPIClient(%s): %v", name, err)
	}
}

// A claim takes no more of a client's API reviews than fit beside those
// already running, oldest first, and another client is not held up.
func TestClaimQueuedCapsAPIClient(t *testing.T) {
	s := newStore(t)
	a := newAPIClient(t, s, "client-a")
	b := newAPIClient(t, s, "client-b")
	setMaxInFlight(t, s, "client-a", 2)
	setMaxInFlight(t, s, "client-b", 2)
	at := func(i int) time.Time { return t0.Add(time.Duration(i) * time.Minute) }
	queue := func(clientID int64, subject string, i int) string {
		t.Helper()
		id := enqueueAPI(t, s, apiRequest(clientID, "acme", subject))
		setRun(t, s, id, `created_at = $2::timestamptz`, at(i))
		return id
	}
	running := queue(a.ID, "s0", 0)
	setRun(t, s, running, `state = 'running', job_name = 'job-0'`)
	a1, a2, a3 := queue(a.ID, "s1", 1), queue(a.ID, "s2", 2), queue(a.ID, "s3", 3)
	b1, b2, b3 := queue(b.ID, "s1", 4), queue(b.ID, "s2", 5), queue(b.ID, "s3", 6)
	claimAt := time.Now().Add(time.Hour)

	got, err := s.ClaimQueued(ctx, 10, claimAt)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{a1, b1, b2}; !slices.Equal(runIDs(got), want) {
		t.Fatalf("claimed %v, want %v: a's oldest beside its running one, and b's two", runIDs(got), want)
	}
	for _, id := range []string{a2, a3, b3} {
		if st := getRun(t, s, id).State; st != Queued {
			t.Errorf("%s = %s, want queued", id, st)
		}
	}
	if got, err := s.ClaimQueued(ctx, 10, claimAt); err != nil || len(got) != 0 {
		t.Errorf("a claim with both clients full = %v, %v; want none", runIDs(got), err)
	}

	// A review that ends makes room for the next, oldest first.
	setRun(t, s, running, `state = 'published'`)
	if got, err := s.ClaimQueued(ctx, 10, claimAt); err != nil || !slices.Equal(runIDs(got), []string{a2}) {
		t.Errorf("after one ended = %v, %v; want %s", runIDs(got), err, a2)
	}

	// A raised cap takes effect on the next claim.
	setMaxInFlight(t, s, "client-b", 3)
	if got, err := s.ClaimQueued(ctx, 10, claimAt); err != nil || !slices.Equal(runIDs(got), []string{b3}) {
		t.Errorf("after b's cap was raised = %v, %v; want %s", runIDs(got), err, b3)
	}
}

// Merge request runs are claimed as before, first in, first out with API
// reviews, and an API review the cap holds takes no slot from them.
func TestClaimQueuedIgnoresCapForMR(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "gateway")
	setMaxInFlight(t, s, "gateway", 1)
	at := func(i int) time.Time { return t0.Add(time.Duration(i) * time.Minute) }
	mr := func(iid int64, i int) string {
		t.Helper()
		id := enqueue(t, s, MRKey{Instance: mr1.Instance, ProjectID: mr1.ProjectID, IID: iid}, "h"+strconv.Itoa(i))
		setRun(t, s, id, `created_at = $2::timestamptz`, at(i))
		return id
	}
	api := func(subject string, i int) string {
		t.Helper()
		id := enqueueAPI(t, s, apiRequest(c.ID, "acme", subject))
		setRun(t, s, id, `created_at = $2::timestamptz`, at(i))
		return id
	}
	m1 := mr(10, 1)
	a1 := api("s1", 2)
	m2 := mr(11, 3)
	a2 := api("s2", 4)
	m3 := mr(12, 5)
	m4 := mr(13, 6)
	claimAt := time.Now().Add(time.Hour)

	got, err := s.ClaimQueued(ctx, 3, claimAt)
	if err != nil || !slices.Equal(runIDs(got), []string{m1, a1, m2}) {
		t.Fatalf("first claim = %v, %v; want %v, oldest first", runIDs(got), err, []string{m1, a1, m2})
	}
	// a2 is older than m3, but a1 runs: it waits and takes no slot.
	got, err = s.ClaimQueued(ctx, 1, claimAt)
	if err != nil || !slices.Equal(runIDs(got), []string{m3}) {
		t.Fatalf("second claim = %v, %v; want %s", runIDs(got), err, m3)
	}
	got, err = s.ClaimQueued(ctx, 10, claimAt)
	if err != nil || !slices.Equal(runIDs(got), []string{m4}) {
		t.Fatalf("third claim = %v, %v; want %s", runIDs(got), err, m4)
	}
	if st := getRun(t, s, a2).State; st != Queued {
		t.Errorf("the held API review = %s, want queued", st)
	}
	setRun(t, s, a1, `state = 'reported'`)
	if got, err := s.ClaimQueued(ctx, 10, claimAt); err != nil || !slices.Equal(runIDs(got), []string{a2}) {
		t.Errorf("once a1 is reported = %v, %v; want %s", runIDs(got), err, a2)
	}
}

// The usage page lists each client's usage with its tenants, busiest first,
// counting by when the usage was reported as UsageByRepository does; a
// client with no usage in the period, and a merge request's usage, are not in
// it.
func TestUsageByAPIClient(t *testing.T) {
	s := newStore(t)
	a := newAPIClient(t, s, "client-a")
	b := newAPIClient(t, s, "client-b")
	newAPIClient(t, s, "client-idle")

	// client-a: acme has two reviews (one with two skills), beta has one.
	seedTenantUsage(t, s, a.ID, "acme", "s1", usageAt(5, 10, 0), skillRow("security", "no_blocking_issues", 100, 20), skillRow("style", "no_blocking_issues", 30, 5))
	seedTenantUsage(t, s, a.ID, "acme", "s2", usageAt(6, 10, 0), skillRow("security", "no_blocking_issues", 10, 0))
	seedTenantUsage(t, s, a.ID, "beta", "s1", usageAt(7, 10, 0), skillRow("security", "no_blocking_issues", 300, 40))
	// Before the period: not counted.
	seedTenantUsage(t, s, a.ID, "acme", "s3", usageSince.Add(-time.Second), skillRow("security", "no_blocking_issues", 9000, 0))
	// client-b is busier, and asks with no tenant.
	seedTenantUsage(t, s, b.ID, "", "s1", usageAt(5, 11, 0), skillRow("security", "no_blocking_issues", 1000, 100))
	// A merge request's usage is a repository's.
	seedUsage(t, s, usageMR(7, 1), usageAt(5, 12, 0), TriggerPush, false, "m", skillRow("security", "no_blocking_issues", 70000, 0))

	// Derived from the rows above. client-b: 1000 in + 100 out = 1,100 tokens.
	// client-a: acme s1 (100+30) in and (20+5) out, acme s2 10 in, so acme is
	// 140 in, 25 out (165 tokens, 2 reviews); beta is 300 in, 40 out (340 tokens,
	// 1 review). client-a is 440 in, 65 out (505 tokens, 3 reviews). The 9000 is
	// before the period and the 70000 is a repository's. Busiest first: client-b
	// (1,100), then client-a (505); beta (340) before acme (165).
	got, err := s.UsageByAPIClient(ctx, usageSince)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "client-b" || got[1].Name != "client-a" {
		t.Fatalf("UsageByAPIClient = %+v, want client-b then client-a, no idle client", got)
	}
	wantTokens(t, "client-b", got[0].Tokens, TokenSum{In: 1000, Out: 100})
	if got[0].Reviews != 1 || len(got[0].Tenants) != 1 || got[0].Tenants[0].Tenant != "" || got[0].Tenants[0].Reviews != 1 {
		t.Errorf("client-b = %+v, want one review of the empty tenant", got[0])
	}
	ca := got[1]
	// Reviews counts a review once, however many skills it ran.
	wantTokens(t, "client-a", ca.Tokens, TokenSum{In: 440, Out: 65})
	if ca.Reviews != 3 || len(ca.Tenants) != 2 {
		t.Fatalf("client-a = %+v, want 3 reviews over 2 tenants", ca)
	}
	// beta used 340, acme 165: busiest first.
	if ca.Tenants[0].Tenant != "beta" || ca.Tenants[0].Reviews != 1 {
		t.Errorf("first tenant = %+v, want beta with one review", ca.Tenants[0])
	}
	wantTokens(t, "beta", ca.Tenants[0].Tokens, TokenSum{In: 300, Out: 40})
	if ca.Tenants[1].Tenant != "acme" || ca.Tenants[1].Reviews != 2 {
		t.Errorf("second tenant = %+v, want acme with two reviews", ca.Tenants[1])
	}
	wantTokens(t, "acme", ca.Tenants[1].Tokens, TokenSum{In: 140, Out: 25})

	// The page's total is its daily chart's: the repositories plus the clients.
	repos, err := s.UsageByRepository(ctx, usageSince)
	if err != nil {
		t.Fatal(err)
	}
	days, err := s.UsageDaily(ctx, usageSince, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var total, daily int64
	for _, r := range repos {
		total += r.Tokens.In + r.Tokens.Out
	}
	for _, c := range got {
		total += c.Tokens.In + c.Tokens.Out
	}
	for _, d := range days {
		daily += d.Tokens.In + d.Tokens.Out
	}
	if total != daily {
		t.Errorf("repositories plus clients = %d tokens, the daily chart = %d", total, daily)
	}
}

// The metrics count each API client's reviews by result, tokens and
// callbacks, by client name and never by tenant.
func TestMetricsTotalsAPIClients(t *testing.T) {
	s := newStore(t)
	a := newCallbackClient(t, s, "client-a")
	b := newAPIClient(t, s, "client-b")
	newAPIClient(t, s, "client-idle")

	finish := func(clientID int64, tenant, subject string, dec RunDecision, skills ...SkillUsage) string {
		t.Helper()
		id := seedTenantUsage(t, s, clientID, tenant, subject, usageAt(5, 10, 0), skills...)
		if err := s.FinishAPIReview(ctx, id, []byte(`{"status":"completed"}`), dec); err != nil {
			t.Fatalf("FinishAPIReview: %v", err)
		}
		return id
	}
	clean := finish(a.ID, "acme", "s1", RunDecision{Outcome: OutcomeNoBlocking}, skillRow("security", "no_blocking_issues", 100, 20))
	finish(a.ID, "beta", "s2", RunDecision{Outcome: OutcomeNoBlocking}, skillRow("security", "no_blocking_issues", 10, 1))
	blocking := finish(a.ID, "acme", "s3", RunDecision{Outcome: OutcomeBlocking}, skillRow("security", "has_blocking_issues", 5, 5))
	finish(b.ID, "", "s1", RunDecision{Outcome: OutcomeNothingReviewed})
	finish(b.ID, "", "s2", RunDecision{Outcome: OutcomeFailed, Reason: "the review failed"})
	// Publishing that failed is a failure; a newer request supersedes the first
	// of its subject; a queued review is not counted yet.
	pf := enqueueAPI(t, s, apiRequest(b.ID, "", "s3"))
	setRun(t, s, pf, `state = 'publish_failed'`)
	old := enqueueAPI(t, s, apiRequest(b.ID, "", "s4"))
	enqueueAPI(t, s, apiRequest(b.ID, "", "s4"))
	if apiState(t, s, old) != Superseded {
		t.Fatal("the first request was not superseded")
	}

	// Callbacks of client-a: the clean review's takes two failed tries, then a
	// 2xx; the blocking review's gives up; one more was given up for want of a URL.
	now := time.Now()
	attempt := func(runID string, att CallbackAttempt, delivered, gaveUp bool) {
		t.Helper()
		state, _ := callbackState(t, s, runID)
		if state == CallbackNone {
			t.Fatalf("review %s has no callback", runID)
		}
		var cb string
		if err := s.pool.QueryRow(ctx, `SELECT id::text FROM api_callbacks WHERE run_id = $1::uuid`, runID).Scan(&cb); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordAPICallbackAttempt(ctx, cb, att, delivered, now.Add(time.Minute), gaveUp); err != nil {
			t.Fatal(err)
		}
	}
	attempt(clean, CallbackAttempt{At: now, StatusCode: 500}, false, false)
	attempt(clean, CallbackAttempt{At: now, Error: "timeout"}, false, false)
	attempt(clean, CallbackAttempt{At: now, StatusCode: 204}, true, false)
	attempt(blocking, CallbackAttempt{At: now, StatusCode: 503}, false, true)
	noURL := finish(a.ID, "acme", "s5", RunDecision{Outcome: OutcomeNoBlocking})
	attempt(noURL, CallbackAttempt{At: now, Error: CallbackNoURL}, false, true)

	// Expected values, from the rows above. Reviews: client-a has no_blocking
	// clean, s2 and s5 (3) and blocking s3 (1); client-b has nothing_reviewed s1
	// (1), failed s2 and the publish_failed s3 (2) and the superseded first s4
	// (1); the second s4 is still queued. Tokens: only client-a's three reviews
	// have skills: in 100+10+5 = 115, out 20+1+5 = 26, over 3 rows. Callbacks of
	// client-a: delivered 1 (clean); failed_attempt 3 (clean's 500 and timeout,
	// blocking's 503); gave_up 1 (blocking); the no-URL one is in neither.
	got, err := s.MetricsTotals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reviews := map[[2]string]int64{}
	for _, r := range got.APIReviews {
		reviews[[2]string{r.Client, r.Result}] += r.Reviews
	}
	wantReviews := map[[2]string]int64{
		{"client-a", OutcomeNoBlocking}: 3, {"client-a", OutcomeBlocking}: 1,
		{"client-b", OutcomeNothingReviewed}: 1, {"client-b", OutcomeFailed}: 2, {"client-b", OutcomeSuperseded}: 1,
	}
	if !reflect.DeepEqual(reviews, wantReviews) {
		t.Errorf("APIReviews = %v, want %v", reviews, wantReviews)
	}

	tokens := map[string]APITokenTotal{}
	for _, r := range got.APITokens {
		tokens[r.Client] = r
	}
	if len(tokens) != 1 || tokens["client-a"].Rows != 3 {
		t.Fatalf("APITokens = %+v, want one row group for client-a over 3 rows", got.APITokens)
	}
	wantTokens(t, "client-a tokens", tokens["client-a"].Tokens, TokenSum{In: 115, Out: 26})

	callbacks := map[[2]string]int64{}
	for _, r := range got.APICallbacks {
		callbacks[[2]string{r.Client, r.Outcome}] += r.Count
	}
	wantCallbacks := map[[2]string]int64{
		{"client-a", CallbackOutcomeDelivered}: 1, {"client-a", CallbackOutcomeFailedAttempt}: 3, {"client-a", CallbackOutcomeGaveUp}: 1,
	}
	if !reflect.DeepEqual(callbacks, wantCallbacks) {
		t.Errorf("APICallbacks = %v, want %v", callbacks, wantCallbacks)
	}
}

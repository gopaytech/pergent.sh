package store

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ChainReviewers is a repository's reviewers 2 to 5 and its judge
// (reviewer_chains). Chain.Slot is SlotOf(n) or SlotJudge. Its rows have
// scope 'repo' and no team, so the chain code reads them as it reads
// model_chains.
var ChainReviewers = ChainSet{chains: "reviewer_chains", entries: "reviewer_chain_entries", slot: "c.slot"}

const (
	// MaxReviewers is the most reviewers a repository has, reviewer 1
	// included.
	MaxReviewers = 5
	// SlotJudge is the judge chain's slot.
	SlotJudge = "judge"
)

// SlotOf is reviewer n's slot, "r2" to "r5".
func SlotOf(n int) string { return "r" + strconv.Itoa(n) }

// RepoReviewers is what a repository stores beyond reviewer 1: Extra[0] is
// reviewer 2. Judge is the judge chain; JudgeFound is false with no row.
type RepoReviewers struct {
	Extra      []Chain
	Judge      Chain
	JudgeFound bool
}

// Unchanged returns the stored chain that reviewer i+2 of a save of extra
// comes from when its entries did not change: the stored reviewer of the
// same slot with the same entries or, when the save has fewer reviewers than
// are stored (a remove), the stored reviewer after it (i+3) with the same
// entries. Such a reviewer keeps that chain's saved_by_admin and updated_at
// and is not checked against the saver's rights. ok is false for a changed
// reviewer: it takes the saver's flag.
func (b RepoReviewers) Unchanged(extra [][]EntryRef, i int) (from Chain, ok bool) {
	if i < len(b.Extra) && slices.Equal(b.Extra[i].Entries, extra[i]) {
		return b.Extra[i], true
	}
	if len(extra) < len(b.Extra) && i+1 < len(b.Extra) && slices.Equal(b.Extra[i+1].Entries, extra[i]) {
		return b.Extra[i+1], true
	}
	return Chain{}, false
}

// JudgeUnchanged is whether a save's judge has the stored judge's entries.
func (b RepoReviewers) JudgeUnchanged(judge []EntryRef) bool {
	return b.JudgeFound && slices.Equal(b.Judge.Entries, judge)
}

var (
	ErrTooManyReviewers      = errors.New("store: at most 5 reviewers")
	ErrEmptyReviewer         = errors.New("store: a reviewer needs an entry")
	ErrNoJudge               = errors.New("store: 2 or more reviewers need a judge")
	ErrJudgeWithoutReviewers = errors.New("store: a judge needs 2 or more reviewers")
)

// reviewersWhere matches one repository's reviewer chains, the project
// ignoring case.
const reviewersWhere = `WHERE c.instance = $1 AND lower(c.project) = lower($2)`

// Reviewers returns the repository's reviewers 2 to 5, in slot order, and
// its judge.
func (s *Store) Reviewers(ctx context.Context, instance, project string) (RepoReviewers, error) {
	return loadReviewers(ctx, s.pool, instance, project)
}

func loadReviewers(ctx context.Context, q rowsQuerier, instance, project string) (RepoReviewers, error) {
	chains, err := loadChains(ctx, q, ChainReviewers, reviewersWhere, instance, project)
	if err != nil {
		return RepoReviewers{}, err
	}
	var r RepoReviewers
	for _, c := range chains {
		if c.Slot == SlotJudge {
			r.Judge, r.JudgeFound = c, true
			continue
		}
		r.Extra = append(r.Extra, c)
	}
	// "r2" to "r5" sort as text.
	slices.SortFunc(r.Extra, func(a, b Chain) int { return strings.Compare(a.Slot, b.Slot) })
	return r, nil
}

// ReviewersSave is a save of the Reviewers card: Extra are reviewers 2.. in
// order, each 1 to MaxChain entries; Judge is required with Extra and empty
// without.
type ReviewersSave struct {
	Instance, Project string
	Extra             [][]EntryRef
	Judge             []EntryRef
	SavedByAdmin      bool
}

// SaveReviewers replaces the repository's slots in one transaction and
// returns them as they were, for the audit. A slot whose entries do not
// change keeps its saved_by_admin and updated_at. After a remove, reviewer
// k is compared with the stored reviewer it comes from (k+1): a moved chain
// with the same entries keeps that reviewer's saved_by_admin
// (RepoReviewers.Unchanged). A team entry that is not there is
// ErrModelNotFound. Who may name which entry is the caller's to check.
func (s *Store) SaveReviewers(ctx context.Context, r ReviewersSave, at time.Time) (before RepoReviewers, err error) {
	if len(r.Extra) > MaxReviewers-1 {
		return RepoReviewers{}, ErrTooManyReviewers
	}
	all := slices.Clone(r.Judge)
	for _, e := range r.Extra {
		if len(e) == 0 {
			return RepoReviewers{}, ErrEmptyReviewer
		}
		if len(e) > MaxChain {
			return RepoReviewers{}, ErrChainTooLong
		}
		all = append(all, e...)
	}
	if len(r.Judge) > MaxChain {
		return RepoReviewers{}, ErrChainTooLong
	}
	if len(r.Extra) > 0 && len(r.Judge) == 0 {
		return RepoReviewers{}, ErrNoJudge
	}
	if len(r.Extra) == 0 && len(r.Judge) > 0 {
		return RepoReviewers{}, ErrJudgeWithoutReviewers
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RepoReviewers{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// As a chain B repository save: the entries, the repository's row, then
	// the chains.
	if err := lockTeamEntries(ctx, tx, all); err != nil {
		return RepoReviewers{}, err
	}
	if _, err := lockRepoRow(ctx, tx, r.Instance, r.Project); err != nil {
		return RepoReviewers{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT c.id FROM `+ChainReviewers.chains+` c `+reviewersWhere+` ORDER BY c.id FOR UPDATE`,
		r.Instance, r.Project); err != nil {
		return RepoReviewers{}, err
	}
	if before, err = loadReviewers(ctx, tx, r.Instance, r.Project); err != nil {
		return RepoReviewers{}, err
	}
	keep := make([]string, 0, len(r.Extra)+1)
	for i := range r.Extra {
		keep = append(keep, SlotOf(i+2))
	}
	if len(r.Extra) > 0 {
		keep = append(keep, SlotJudge)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM `+ChainReviewers.chains+` c `+reviewersWhere+` AND NOT (c.slot = ANY($3))`,
		r.Instance, r.Project, keep); err != nil {
		return RepoReviewers{}, err
	}
	for i, entries := range r.Extra {
		slot := SlotOf(i + 2)
		byAdmin, updated := r.SavedByAdmin, at
		if from, ok := before.Unchanged(r.Extra, i); ok {
			if from.Slot == slot {
				continue
			}
			byAdmin, updated = from.SavedByAdmin, from.UpdatedAt
		}
		if err := writeReviewerChain(ctx, tx, r.Instance, r.Project, slot, byAdmin, updated, entries); err != nil {
			return RepoReviewers{}, err
		}
	}
	if len(r.Extra) > 0 && !before.JudgeUnchanged(r.Judge) {
		if err := writeReviewerChain(ctx, tx, r.Instance, r.Project, SlotJudge, r.SavedByAdmin, at, r.Judge); err != nil {
			return RepoReviewers{}, err
		}
	}
	return before, tx.Commit(ctx)
}

// writeReviewerChain stores one slot of a repository, creating its row when
// there is none.
func writeReviewerChain(ctx context.Context, tx pgx.Tx, instance, project, slot string, byAdmin bool, at time.Time, entries []EntryRef) error {
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO `+ChainReviewers.chains+` (instance, project, slot, saved_by_admin, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (instance, lower(project), slot)
		DO UPDATE SET saved_by_admin = EXCLUDED.saved_by_admin, updated_at = EXCLUDED.updated_at
		RETURNING id`, instance, project, slot, byAdmin, at).Scan(&id); err != nil {
		return err
	}
	return writeChainEntries(ctx, tx, ChainReviewers, id, entries)
}

// compactRemoved compacts each repository of chains once, in order, and
// gives each chain still there the slot it moved to.
func compactRemoved(ctx context.Context, tx pgx.Tx, chains []Chain, at time.Time) error {
	type repo struct{ instance, project string }
	var done []repo
	ids := make([]int64, 0, len(chains))
	for _, c := range chains {
		ids = append(ids, c.ID)
		if slices.ContainsFunc(done, func(r repo) bool { return r.instance == c.Instance && strings.EqualFold(r.project, c.Project) }) {
			continue
		}
		done = append(done, repo{c.Instance, c.Project})
		if err := compactReviewers(ctx, tx, c.Instance, c.Project, at); err != nil {
			return err
		}
	}
	rows, err := tx.Query(ctx, `SELECT id, slot FROM `+ChainReviewers.chains+` WHERE id = ANY($1)`, ids)
	if err != nil {
		return err
	}
	slots := map[int64]string{}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var slot string
		if err := rows.Scan(&id, &slot); err != nil {
			return err
		}
		slots[id] = slot
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range chains {
		if slot, ok := slots[chains[i].ID]; ok {
			chains[i].Slot = slot
		}
	}
	return nil
}

// compactReviewers deletes the repository's empty r-slots, moves the others
// down to r2, r3, ... in order, and deletes the judge row when no r-slot is
// left. An empty judge with r-slots left stays (the card warns; reviews run
// with --no-judge). A moved slot keeps its saved_by_admin; at is its
// updated_at. The slots move in ascending order, one row per statement, as
// the unique index is checked per row.
func compactReviewers(ctx context.Context, tx pgx.Tx, instance, project string, at time.Time) error {
	if _, err := tx.Exec(ctx, `DELETE FROM `+ChainReviewers.chains+` c `+reviewersWhere+` AND c.slot <> $3
		AND NOT EXISTS (SELECT 1 FROM `+ChainReviewers.entries+` e WHERE e.chain_id = c.id)`, instance, project, SlotJudge); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT c.id, c.slot FROM `+ChainReviewers.chains+` c `+reviewersWhere+` AND c.slot <> $3 ORDER BY c.slot`,
		instance, project, SlotJudge)
	if err != nil {
		return err
	}
	type slotRow struct {
		id   int64
		slot string
	}
	left, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (slotRow, error) {
		var r slotRow
		err := row.Scan(&r.id, &r.slot)
		return r, err
	})
	if err != nil {
		return err
	}
	for i, r := range left {
		if want := SlotOf(i + 2); r.slot != want {
			if _, err := tx.Exec(ctx, `UPDATE `+ChainReviewers.chains+` SET slot = $2, updated_at = $3 WHERE id = $1`, r.id, want, at); err != nil {
				return err
			}
		}
	}
	if len(left) == 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM `+ChainReviewers.chains+` c `+reviewersWhere+` AND c.slot = $3`,
			instance, project, SlotJudge); err != nil {
			return err
		}
	}
	return nil
}

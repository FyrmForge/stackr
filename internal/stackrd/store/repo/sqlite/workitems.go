package sqlite

import (
	"context"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func (s *Store) CreateWorkItem(ctx context.Context, w *repo.WorkItem) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO work_items (id, kind, dedupe_key, payload, status, step, progress, error, attempts, created_at, started_at, finished_at)
		 VALUES (:id, :kind, :dedupe_key, :payload, :status, :step, :progress, :error, :attempts, :created_at, :started_at, :finished_at)`, w)
	return err
}

func (s *Store) GetWorkItem(ctx context.Context, id string) (*repo.WorkItem, error) {
	return get[repo.WorkItem](ctx, s, `SELECT * FROM work_items WHERE id = ?`, id)
}

// ListWorkItemsByStatus is oldest first: the runner takes them in the order
// they were asked for, and boot recovery walks the rows a dead process left
// behind in the same order.
func (s *Store) ListWorkItemsByStatus(ctx context.Context, status string) ([]repo.WorkItem, error) {
	return list[repo.WorkItem](ctx, s,
		`SELECT * FROM work_items WHERE status = ? ORDER BY rowid`, status)
}

// LatestWorkItem is the newest item of one kind and key, whatever its status.
// The plan page polls this to find the apply it just asked for without having
// to remember an id across a redirect.
func (s *Store) LatestWorkItem(ctx context.Context, kind, dedupeKey string) (*repo.WorkItem, error) {
	return get[repo.WorkItem](ctx, s,
		`SELECT * FROM work_items WHERE kind = ? AND dedupe_key = ? ORDER BY rowid DESC LIMIT 1`,
		kind, dedupeKey)
}

// SupersedeQueuedWorkItems marks every waiting item of this kind and key
// superseded, except the one just enqueued. Only queued rows: a running job is
// already changing containers and databases, and abandoning it halfway is
// worse than letting it finish and be overwritten by the next one.
//
// Older only, by rowid. "Everything but me" is not the same rule: two enqueues
// landing together each superseded the other and neither job ever ran, because
// ids are UUIDs and carry no order. rowid is already the queue order
// (ListWorkItemsByStatus, LatestWorkItem), so the later row wins and the
// earlier one is what gets dropped.
func (s *Store) SupersedeQueuedWorkItems(ctx context.Context, kind, dedupeKey, exceptID string) error {
	if dedupeKey == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE work_items SET status = 'superseded', finished_at = ?
		 WHERE kind = ? AND dedupe_key = ? AND status = 'queued'
		   AND rowid < (SELECT rowid FROM work_items WHERE id = ?)`,
		time.Now().UTC(), kind, dedupeKey, exceptID)
	return err
}

// ClaimWorkItem moves one queued item to running, reporting whether this
// caller got it. The rows-affected check is the whole lock: two claimers race
// on the same UPDATE and exactly one sees a row change.
//
// One panel process today, so this is enough, and it stays correct if a second
// ever appears, which matters because panel failover is on the roadmap
// (docs/notes.md, misc). No leases until there is a second writer.
func (s *Store) ClaimWorkItem(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE work_items SET status = 'running', started_at = ?, attempts = attempts + 1
		 WHERE id = ? AND status = 'queued'`, time.Now().UTC(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// FinishWorkItem records how an item ended. errMsg empty means it worked.
func (s *Store) FinishWorkItem(ctx context.Context, id, status, errMsg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE work_items SET status = ?, error = ?, finished_at = ? WHERE id = ?`,
		status, errMsg, time.Now().UTC(), id)
	return err
}

// RequeueWorkItem puts a running row back in the queue. Boot recovery uses it
// for the kinds that are safe to run again from the top.
func (s *Store) RequeueWorkItem(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE work_items SET status = 'queued', started_at = NULL WHERE id = ?`, id)
	return err
}

// SetWorkItemProgress writes the coarse step and the progress blob. Called
// from inside a running job, often, so it touches nothing else on the row.
func (s *Store) SetWorkItemProgress(ctx context.Context, id, step, progress string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE work_items SET step = ?, progress = ? WHERE id = ?`, step, progress, id)
	return err
}

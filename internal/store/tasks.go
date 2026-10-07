package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"agriheat/internal/model"
)

// Task is one durable recomputation unit.
type Task struct {
	ID        int64
	PlotID    int64
	AsOf      model.Date
	Start     model.Date
	ChangeID  string
	Reason    string
	IsInitial bool
	RunAfter  model.Date
	Attempts  int
	Err       string
}

// EnqueueTask inserts a recompute task idempotently on (plot_id, change_id).
// An already-existing change id is a no-op, which collapses bursts of reports
// belonging to the same data change into one task.
func (t *Tx) EnqueueTask(k Task) error {
	var runAfter, startArg interface{}
	if !k.RunAfter.IsZero() {
		runAfter = dateArg(k.RunAfter)
	}
	if !k.Start.IsZero() {
		startArg = dateArg(k.Start)
	}
	_, err := t.tx.Exec(t.ctx, `
		INSERT INTO recompute_tasks(plot_id, as_of, start_day, change_id, reason, is_initial, run_after, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'pending')
		ON CONFLICT (plot_id, change_id) DO NOTHING`,
		k.PlotID, dateArg(k.AsOf), startArg, k.ChangeID, k.Reason, k.IsInitial, runAfter)
	return err
}

// EnqueueOrMergeTask is retained for callers that intentionally want the old
// burst-coalescing behavior. The write paths do not use it: distinct data
// changes need their own tasks so event vectors do not depend on worker speed.
// Initial baselines are established synchronously and must never be overwritten
// by a later change merged into a still-pending initial task.
func (t *Tx) EnqueueOrMergeTask(k Task) error {
	if k.IsInitial {
		return t.EnqueueTask(k)
	}
	// Lock the plot row so two batches enqueueing for the same plot serialize
	// their merge decisions.
	if _, err := t.tx.Exec(t.ctx,
		`SELECT id FROM plots WHERE id=$1 FOR UPDATE`, k.PlotID); err != nil {
		return err
	}
	var id int64
	var asOf, startDay pgtype.Date
	err := t.tx.QueryRow(t.ctx, `
		SELECT id, as_of, start_day FROM recompute_tasks
		WHERE plot_id=$1 AND status='pending' AND NOT is_initial
		  AND (run_after IS NULL OR run_after <= current_date)
		ORDER BY id LIMIT 1
		FOR UPDATE`, k.PlotID).Scan(&id, &asOf, &startDay)
	if errors.Is(err, pgx.ErrNoRows) {
		return t.EnqueueTask(k)
	}
	if err != nil {
		return err
	}
	newAsOf := scanDate(asOf)
	if k.AsOf.After(newAsOf) {
		newAsOf = k.AsOf
	}
	newStart := k.Start
	if startDay.Valid {
		if sd := scanDate(startDay); newStart.IsZero() || sd.Before(newStart) {
			newStart = sd
		}
	}
	_, err = t.tx.Exec(t.ctx, `
		UPDATE recompute_tasks
		SET as_of=$2, start_day=$3, change_id=$4,
		    reason = reason || '; ' || $5
		WHERE id=$1`, id, dateArg(newAsOf), dateArg(newStart), k.ChangeID, k.Reason)
	return err
}

// Claim atomically marks one due pending task as processing and returns the
// open transaction holding it. Caller must Commit (Complete) or Rollback; on
// rollback the row lock is released and its status change is undone.
// Returns (nil tx, zero task, nil) when nothing is due.
func (db *DB) Claim(ctx context.Context) (*Tx, Task, error) {
	t, err := db.Begin(ctx)
	if err != nil {
		return nil, Task{}, err
	}
	row := t.tx.QueryRow(t.ctx, `
		UPDATE recompute_tasks
		SET status='processing', started_at=now(), attempts=attempts+1
		WHERE id = (
			SELECT id FROM recompute_tasks candidate
			WHERE candidate.status='pending'
			  AND (candidate.run_after IS NULL OR candidate.run_after <= current_date)
			  AND NOT EXISTS (
			      SELECT 1 FROM recompute_tasks active
			      WHERE active.plot_id = candidate.plot_id
			        AND (
			            active.status='processing'
			            OR (active.status='pending'
			                AND (active.run_after IS NULL OR active.run_after <= current_date))
			        )
			        AND active.id < candidate.id
			  )
			ORDER BY candidate.id
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, plot_id, as_of, COALESCE(start_day, as_of), change_id, reason, is_initial, run_after, attempts`)
	var k Task
	var asOf, startDay, runAfter pgtype.Date
	err = row.Scan(&k.ID, &k.PlotID, &asOf, &startDay, &k.ChangeID, &k.Reason,
		&k.IsInitial, &runAfter, &k.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Rollback()
		return nil, Task{}, nil
	}
	if err != nil {
		t.Rollback()
		return nil, Task{}, err
	}
	k.AsOf = scanDate(asOf)
	k.Start = scanDate(startDay)
	if runAfter.Valid {
		k.RunAfter = scanDate(runAfter)
	}
	return t, k, nil
}

// Complete marks a claimed task done (in its own transaction).
func (t *Tx) Complete(id int64) error {
	_, err := t.tx.Exec(t.ctx, `
		UPDATE recompute_tasks
		SET status='done', finished_at=now(), err=''
		WHERE id=$1`, id)
	return err
}

// EnqueueRollforward enqueues one daily roll-forward task for each plot so
// projections advance with the calendar even on days with no new data. The
// change id is unique per plot/day, so at-most-one task per day.
func (db *DB) EnqueueRollforward(ctx context.Context, today model.Date) (int, error) {
	t, err := db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer t.Rollback()
	ids, err := t.AllPlotIDs()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, pid := range ids {
		err := t.EnqueueTask(Task{
			PlotID: pid, AsOf: today, Start: today,
			ChangeID: "rollforward:" + today.String(),
			Reason:   "daily roll-forward",
		})
		if err != nil {
			return 0, err
		}
		n++
	}
	if err := t.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// Fail records an error and requeues as pending for retry.
func (t *Tx) Fail(id int64, cause error) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	_, err := t.tx.Exec(t.ctx, `
		UPDATE recompute_tasks
		SET status='pending', started_at=NULL, err=$2
		WHERE id=$1`, id, truncate(msg, 1000))
	return err
}

// FailDead parks a task after MaxAttempts so it does not loop forever.
func (t *Tx) FailDead(id int64, cause error) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	_, err := t.tx.Exec(t.ctx, `
		UPDATE recompute_tasks
		SET status='failed', err=$2 WHERE id=$1`, id, truncate(msg, 1000))
	return err
}

// RecoverProcessing resets processing tasks to pending at startup so a crash
// mid-task resumes and yields the same result as an uninterrupted run.
func (db *DB) RecoverProcessing(ctx context.Context) error {
	_, err := db.Pool.Exec(ctx, `
		UPDATE recompute_tasks SET status='pending', started_at=NULL
		WHERE status='processing'`)
	return err
}

// PendingCount returns the number of currently due pending tasks.
func (db *DB) PendingCount(ctx context.Context) (int, error) {
	var n int
	err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM recompute_tasks
		 WHERE status='pending' AND (run_after IS NULL OR run_after <= current_date)`).
		Scan(&n)
	return n, err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

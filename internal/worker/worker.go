// Package worker drains the durable recompute task queue.
//
// Tasks are claimed with SELECT ... FOR UPDATE SKIP LOCKED so multiple app
// replicas never process one task twice. The claimed row is released
// immediately (rollback of the claim) while the plot recomputation runs in a
// separate transaction; a crash at any point leaves the task 'pending'
// (RecoverProcessing resets in-flight rows at startup), and re-running the
// same task is idempotent thanks to engine snapshots and the event unique key.
package worker

import (
	"context"
	"log"
	"time"

	"agriheat/internal/engine"
	"agriheat/internal/model"
	"agriheat/internal/store"
)

// MaxAttempts bounds retries for poison tasks.
const MaxAttempts = 10

// Worker polls the queue.
type Worker struct {
	DB     *store.DB
	Engine engine.DB
	Poll   time.Duration
}

// New builds a worker over db.
func New(db *store.DB, poll time.Duration) *Worker {
	if poll == 0 {
		poll = 200 * time.Millisecond
	}
	return &Worker{DB: db, Engine: db, Poll: poll}
}

// Run blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	if err := w.DB.RecoverProcessing(ctx); err != nil {
		log.Printf("worker: recover processing: %v", err)
	}
	w.rollForward(ctx) // once at startup (deduped by change id)

	ticker := time.NewTicker(w.Poll)
	defer ticker.Stop()
	dayTicker := time.NewTicker(time.Hour)
	defer dayTicker.Stop()
	lastDay := model.Today()
	for {
		select {
		case <-ctx.Done():
			return
		case <-dayTicker.C:
			if today := model.Today(); today != lastDay {
				lastDay = today
				w.rollForward(ctx)
			}
		default:
		}
		worked, err := w.tick(ctx)
		if err != nil {
			log.Printf("worker: tick: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		if !worked {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}
}

// rollForward enqueues the daily projection advance for every plot.
func (w *Worker) rollForward(ctx context.Context) {
	if n, err := w.DB.EnqueueRollforward(ctx, model.Today()); err != nil {
		log.Printf("worker: rollforward enqueue: %v", err)
	} else if n > 0 {
		log.Printf("worker: enqueued %d daily roll-forward tasks", n)
	}
}

// tick processes at most one task; worked=false when the queue was empty.
func (w *Worker) tick(ctx context.Context) (bool, error) {
	claimTx, task, err := w.DB.Claim(ctx)
	if err != nil {
		return false, err
	}
	if claimTx == nil {
		return false, nil
	}
	// Persist the 'processing' marker and release the row lock. Other workers
	// only select status='pending', so the task cannot be processed twice; if
	// we crash after this commit, startup RecoverProcessing requeues it.
	if err := claimTx.Commit(); err != nil {
		return true, err
	}

	req := engine.Request{
		PlotID:   task.PlotID,
		AsOf:     task.AsOf,
		Start:    task.Start,
		ChangeID: task.ChangeID,
		Reason:   task.Reason,
		Initial:  task.IsInitial,
	}
	if _, err := engine.Recompute(w.Engine, req); err != nil {
		return true, w.fail(ctx, task.ID, task.Attempts, err)
	}
	if err := w.finish(ctx, task.ID); err != nil {
		return true, err
	}
	return true, nil
}

func (w *Worker) finish(ctx context.Context, id int64) error {
	t, err := w.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer t.Rollback()
	if err := t.Complete(id); err != nil {
		return err
	}
	return t.Commit()
}

func (w *Worker) fail(ctx context.Context, id int64, attempts int, cause error) error {
	log.Printf("worker: task %d failed (attempt %d): %v", id, attempts, cause)
	t, err := w.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer t.Rollback()
	if attempts >= MaxAttempts {
		if err := t.FailDead(id, cause); err != nil {
			return err
		}
	} else {
		if err := t.Fail(id, cause); err != nil {
			return err
		}
	}
	return t.Commit()
}

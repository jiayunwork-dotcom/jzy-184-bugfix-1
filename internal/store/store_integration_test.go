//go:build integration

// PostgreSQL-backed integration tests. Run with the database from
// docker-compose up:
//
//	TEST_DATABASE_URL=postgres://agri:agri@localhost:5432/agriheat \
//	    go test -tags=integration ./internal/store/
package store_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"agriheat/internal/model"
	"agriheat/internal/store"
)

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := "postgres://agri:agri@localhost:5432/agriheat?sslmode=disable"
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		dsn = v
	}
	return dsn
}

func openDB(t *testing.T) *store.DB {
	t.Helper()
	dsn := testDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := store.Open(ctx, dsn)
	if err != nil {
		t.Skipf("postgres not available (%v); run with docker-compose", err)
	}
	t.Cleanup(func() {
		cleanTables(dsn)
		db.Close()
	})
	return db
}

func cleanTables(dsn string) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return
	}
	defer pool.Close()
	for _, tbl := range []string{
		"recompute_tasks", "stage_events", "plot_stages", "plot_days",
		"plot_snapshots", "weather_basis", "weather", "normals",
		"plot_bindings", "plots", "varieties", "stations",
	} {
		_, _ = pool.Exec(ctx, "TRUNCATE "+tbl+" RESTART IDENTITY CASCADE")
	}
}

// TestClaimSkipsLocked proves two concurrent claimers get different (or one
// nil) tasks via FOR UPDATE SKIP LOCKED.
func TestClaimSkipsLocked(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sid, err := tx.CreateStation(&model.Station{Code: "S1", Name: "s"})
	if err != nil {
		t.Fatal(err)
	}
	vid, err := tx.CreateVariety(&model.Variety{Name: "v", BaseTemp: 10, CeilingTemp: 35,
		StageRequire: map[model.Stage]float64{
			model.StageEmergence: 10, model.StageJointing: 30, model.StageTasseling: 60,
			model.StageSilking: 90, model.StageMaturity: 120,
		}})
	if err != nil {
		t.Fatal(err)
	}
	pid, err := tx.CreatePlot(&model.Plot{Code: "P1", VarietyID: vid, SowingDate: model.NewDate(2026, 4, 1)}, sid)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.EnqueueTask(store.Task{PlotID: pid, AsOf: model.Today(), ChangeID: "c1", Reason: "r"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	first, task1, err := db.Claim(ctx)
	if err != nil || first == nil {
		t.Fatalf("first claim: %v %+v", err, task1)
	}
	defer first.Rollback()

	second, _, err := db.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second != nil {
		second.Rollback()
		t.Fatal("second claim must skip the already-locked task")
	}
}

// TestDataChangeNeverMergesIntoInitialTask guards the event-drift fix at the
// queue level: a data-change task queued while the plot's initial (baseline)
// task is still pending must NOT be collapsed into it. The old merge could
// overwrite the initial task's change id while leaving is_initial=true, which
// silently suppressed the first prediction's events ("register then
// immediately batch" rhythm).
func TestDataChangeNeverMergesIntoInitialTask(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sid, err := tx.CreateStation(&model.Station{Code: "MI", Name: "m"})
	if err != nil {
		t.Fatal(err)
	}
	vid, err := tx.CreateVariety(&model.Variety{Name: "v", BaseTemp: 10, CeilingTemp: 30,
		StageRequire: map[model.Stage]float64{
			model.StageEmergence: 100, model.StageJointing: 300, model.StageTasseling: 600,
			model.StageSilking: 700, model.StageMaturity: 1200,
		}})
	if err != nil {
		t.Fatal(err)
	}
	pid, err := tx.CreatePlot(&model.Plot{Code: "PM", VarietyID: vid, SowingDate: model.NewDate(2026, 5, 1)}, sid)
	if err != nil {
		t.Fatal(err)
	}
	day := model.NewDate(2026, 5, 10)
	if err := tx.EnqueueTask(store.Task{
		PlotID: pid, AsOf: day, ChangeID: "baseline", Reason: "plot registered", IsInitial: true,
	}); err != nil {
		t.Fatal(err)
	}
	// Data lands before the baseline task was claimed: it must stay separate.
	if err := tx.EnqueueOrMergeTask(store.Task{
		PlotID: pid, AsOf: day, Start: model.NewDate(2026, 5, 1),
		ChangeID: "ingest:1", Reason: "batch observation ingest",
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Pool.Query(ctx, `
		SELECT change_id, is_initial FROM recompute_tasks
		WHERE plot_id=$1 ORDER BY id`, pid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type taskRow struct {
		changeID string
		initial  bool
	}
	var got []taskRow
	for rows.Next() {
		var r taskRow
		if err := rows.Scan(&r.changeID, &r.initial); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) != 2 {
		t.Fatalf("data change must not merge into the pending initial task; want 2 rows, got %d: %v", len(got), got)
	}
	if got[0].changeID != "baseline" || !got[0].initial {
		t.Fatalf("baseline task altered: %+v", got[0])
	}
	if got[1].changeID != "ingest:1" || got[1].initial {
		t.Fatalf("data change must remain a separate non-initial task: %+v", got[1])
	}

	// Two ordinary data changes still collapse into one non-initial task.
	c, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Rollback()
	if err := c.EnqueueOrMergeTask(store.Task{
		PlotID: pid, AsOf: day, Start: model.NewDate(2026, 5, 2),
		ChangeID: "ingest:2", Reason: "second batch",
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Commit(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM recompute_tasks WHERE plot_id=$1 AND status='pending'`, pid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("ordinary data changes must still merge; want 2 pending rows, got %d", n)
	}
}

// TestConcurrentSameStationDay applies the same station/day under two
// concurrent transactions with different seqs; after commit the largest seq
// must be the single authoritative row.
func TestConcurrentSameStationDay(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sid, err := tx.CreateStation(&model.Station{Code: "CS", Name: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	day := model.NewDate(2026, 5, 1)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	run := func(seq int64, tx2, tn float64) {
		defer wg.Done()
		c, err := db.Begin(ctx)
		if err != nil {
			errs <- err
			return
		}
		defer c.Rollback()
		if _, _, err := c.UpsertObs(model.Observation{StationID: sid, Date: day, TMax: tx2, TMin: tn, Seq: seq}); err != nil {
			errs <- err
			return
		}
		if err := c.Commit(); err != nil {
			errs <- err
		}
	}
	wg.Add(2)
	go run(1, 20, 8)
	go run(2, 26, 12)
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	c, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Rollback()
	got, err := c.LookupObs(sid, day)
	if err != nil {
		t.Fatal(err)
	}
	if got.Seq != 2 || got.TMax != 26 {
		t.Fatalf("winning row = %+v, want seq 2 tmax 26", got)
	}
}

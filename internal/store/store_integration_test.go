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

// TestClaimPreservesPerPlotOrder ensures a newer task for a plot cannot be
// claimed while an older task for the same plot is still active; events are a
// per-plot history and must be applied in committed (id) order.
func TestClaimPreservesPerPlotOrder(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sid, err := tx.CreateStation(&model.Station{Code: "ORD", Name: "o"})
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
	p1, err := tx.CreatePlot(&model.Plot{Code: "O1", VarietyID: vid, SowingDate: model.NewDate(2026, 4, 1)}, sid)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := tx.CreatePlot(&model.Plot{Code: "O2", VarietyID: vid, SowingDate: model.NewDate(2026, 4, 1)}, sid)
	if err != nil {
		t.Fatal(err)
	}
	asOf := model.NewDate(2026, 6, 1)
	mk := func(plot int64, id string) {
		if err := tx.EnqueueTask(store.Task{PlotID: plot, AsOf: asOf, ChangeID: id, Reason: id}); err != nil {
			t.Fatal(err)
		}
	}
	mk(p1, "p1-older")
	mk(p2, "p2-other")
	mk(p1, "p1-newer")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Globally oldest task belongs to p1.
	claim1, task1, err := db.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if task1.PlotID != p1 || task1.ChangeID != "p1-older" {
		t.Fatalf("first claim = plot %d %s", task1.PlotID, task1.ChangeID)
	}

	// While p1-older is processing, p1-newer must wait even though p2 has a
	// due task; another plot is still claimable. Commit the claim (status now
	// 'processing', lock released) just like the worker does before recompute.
	if err := claim1.Commit(); err != nil {
		t.Fatal(err)
	}

	claim2, task2, err := db.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if task2.PlotID != p2 {
		claim2.Rollback()
		t.Fatalf("second claim should advance the independent plot, got plot %d %s", task2.PlotID, task2.ChangeID)
	}
	if err := claim2.Commit(); err != nil {
		t.Fatal(err)
	}

	claim3, task3, err := db.Claim(ctx)
	if err != nil || claim3 != nil {
		if claim3 != nil {
			claim3.Rollback()
		}
		t.Fatalf("p1-newer must wait for p1-older, got %+v err=%v", task3, err)
	}

	// Finish the older p1 task; the newer one becomes claimable.
	done, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := done.Complete(task1.ID); err != nil {
		t.Fatal(err)
	}
	if err := done.Commit(); err != nil {
		t.Fatal(err)
	}
	claim4, task4, err := db.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer claim4.Rollback()
	if task4.PlotID != p1 || task4.ChangeID != "p1-newer" {
		t.Fatalf("final claim = plot %d %s, want p1-newer", task4.PlotID, task4.ChangeID)
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

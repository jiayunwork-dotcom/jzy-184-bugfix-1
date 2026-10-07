//go:build integration

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"agriheat/internal/api"
	"agriheat/internal/model"
	"agriheat/internal/service"
	"agriheat/internal/store"
	"agriheat/internal/worker"
)

type e2e struct {
	t   *testing.T
	srv *httptest.Server
	db  *store.DB
	svc *service.Service
}

func newE2E(t *testing.T) *e2e {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://agri:agri@localhost:5432/agriheat?sslmode=disable"
	}
	ctx := context.Background()
	db, err := store.Open(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() {
		cleanup(t, db, dsn)
		db.Close()
	})

	svc := service.New(db)
	today, _ := model.ParseDate("2026-06-10")
	svc.Now = func() model.Date { return today }
	svc.NowTS = func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) }

	h := &api.Handler{Svc: svc, DB: db}
	srv := httptest.NewServer(api.NewRouter(h))
	t.Cleanup(srv.Close)

	e := &e2e{t: t, srv: srv, db: db, svc: svc}

	w := worker.New(db, 20*time.Millisecond)
	wctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go w.Run(wctx)
	return e
}

func cleanup(t *testing.T, db *store.DB, dsn string) {
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, `
		TRUNCATE recompute_tasks, stage_events, plot_stages, plot_days,
		         plot_snapshots, weather_basis, weather, normals,
		         plot_bindings, plots, varieties, stations RESTART IDENTITY CASCADE`); err != nil {
		t.Logf("cleanup: %v", err)
	}
}

func (e *e2e) waitEmpty() {
	ctx := context.Background()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		n, err := e.db.PendingCount(ctx)
		if err != nil {
			e.t.Fatal(err)
		}
		if n == 0 {
			return
		}
		time.Sleep(40 * time.Millisecond)
	}
	e.t.Fatal("worker did not drain the queue")
}

func (e *e2e) post(path string, body any) (int, map[string]any) {
	b, _ := json.Marshal(body)
	resp, err := http.Post(e.srv.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *e2e) get(path string) map[string]any {
	resp, err := http.Get(e.srv.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		e.t.Fatalf("GET %s -> %d", path, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func idOf(m map[string]any) int64 { return int64(m["id"].(float64)) }

func TestInitialBaselineDoesNotRaceObservationBatch(t *testing.T) {
	e := newE2E(t)
	today, _ := model.ParseDate("2026-06-30")
	e.svc.Now = func() model.Date { return today }
	e.svc.NowTS = func() time.Time { return time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC) }
	ctx := context.Background()

	sowing := model.NewDate(2026, 5, 1)
	vid, err := e.svc.RegisterVariety(ctx, &model.Variety{
		Name: fmt.Sprintf("race-v-%d", time.Now().UnixNano()), BaseTemp: 10, CeilingTemp: 30,
		StageRequire: map[model.Stage]float64{
			model.StageEmergence: 100,
			model.StageJointing:  300,
			model.StageTasseling: 600,
			model.StageSilking:   700,
			model.StageMaturity:  1200,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	want := map[model.Stage]string{
		model.StageEmergence: "2026-05-10",
		model.StageJointing:  "2026-05-28",
		model.StageTasseling: "2026-06-24",
	}

	runOne := func(n int, delayBeforeBatch time.Duration) int64 {
		sid, err := e.svc.RegisterStation(ctx, &model.Station{
			Code: fmt.Sprintf("race-s-%d-%d", time.Now().UnixNano(), n), Name: "race",
		})
		if err != nil {
			t.Fatal(err)
		}
		pid, err := e.svc.RegisterPlot(ctx, &model.Plot{
			Code:      fmt.Sprintf("race-p-%d-%d", time.Now().UnixNano(), n),
			VarietyID: vid, SowingDate: sowing,
		}, sid)
		if err != nil {
			t.Fatal(err)
		}
		// The initial projection is synchronous and silent; there is no sleep to
		// let a background initial task finish before the first batch.
		if delayBeforeBatch != 0 {
			time.Sleep(delayBeforeBatch)
		}
		rows := make([]service.ObservationIn, 0, today.Sub(sowing)+1)
		for day := sowing; !day.After(today); day = day.AddDays(1) {
			rows = append(rows, service.ObservationIn{
				StationID: sid, Date: day.String(), TMax: 28, TMin: 14, Seq: 1,
			})
		}
		res, err := e.svc.IngestObservations(ctx, rows)
		if err != nil {
			t.Fatal(err)
		}
		if res.Enqueued != 1 {
			t.Fatalf("scenario %d enqueued %d plots", n, res.Enqueued)
		}
		e.waitEmpty()
		return pid
	}

	pidFast := runOne(1, 0)
	pidSlow := runOne(2, 100*time.Millisecond)
	for _, pid := range []int64{pidFast, pidSlow} {
		all, err := store.QueryEvents(ctx, e.db, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		got := map[model.Stage]model.StageEvent{}
		for _, ev := range all {
			if ev.PlotID != pid {
				continue
			}
			if _, exists := got[ev.Stage]; exists {
				t.Fatalf("plot %d duplicate event for %s", pid, ev.Stage)
			}
			got[ev.Stage] = ev
		}
		if len(got) != len(want) {
			t.Fatalf("plot %d got %d events, want %d: %+v", pid, len(got), len(want), got)
		}
		for st, day := range want {
			ev := got[st]
			if ev.OldDate != nil {
				t.Fatalf("plot %d %s old=%s, want null", pid, st, ev.OldDate)
			}
			if ev.NewDate == nil || ev.NewDate.String() != day {
				t.Fatalf("plot %d %s new=%v, want %s", pid, st, ev.NewDate, day)
			}
		}
	}
}

func TestEndToEndHappyPath(t *testing.T) {
	e := newE2E(t)

	st, b := e.post("/api/v1/stations", map[string]any{
		"code": fmt.Sprintf("S-%d", time.Now().UnixNano()), "name": "s",
		"lat": 30.0, "lon": 120.0, "elev": 50.0,
	})
	if st != 201 {
		t.Fatalf("station %d %v", st, b)
	}
	sid := idOf(b)

	st, b = e.post("/api/v1/varieties", map[string]any{
		"name": "m1", "base_temp": 10, "ceiling_temp": 35,
		"stage_require": map[string]float64{
			"emergence": 12, "jointing": 200, "tasseling": 500,
			"silking": 800, "maturity": 1200,
		},
	})
	if st != 201 {
		t.Fatalf("variety %d %v", st, b)
	}
	vid := idOf(b)

	st, b = e.post("/api/v1/plots", map[string]any{
		"code":       fmt.Sprintf("P-%d", time.Now().UnixNano()),
		"variety_id": vid, "station_id": sid, "sowing_date": "2026-05-01",
	})
	if st != 201 {
		t.Fatalf("plot %d %v", st, b)
	}
	pid := idOf(b)
	e.waitEmpty()

	// Batch: 20 warm days at 30/14 (12 GDD/day) plus one illegal row.
	rows := []map[string]any{}
	for k := 0; k < 20; k++ {
		day := model.NewDate(2026, 5, 1).AddDays(k)
		rows = append(rows, map[string]any{
			"station_id": sid, "date": day.String(),
			"tmax": 30, "tmin": 14, "seq": 1,
		})
	}
	rows = append(rows, map[string]any{ // illegal: tmin > tmax
		"station_id": sid, "date": "2026-05-25", "tmax": 5, "tmin": 9, "seq": 1,
	})
	st, b = e.post("/api/v1/observations/batch", map[string]any{"rows": rows})
	if st != 200 {
		t.Fatalf("batch %d %v", st, b)
	}
	if b["accepted"].(float64) != 20 {
		t.Fatalf("accepted=%v want 20", b["accepted"])
	}
	bad := b["results"].([]any)[20].(map[string]any)
	if bad["ok"] != false || bad["reason"] == "" {
		t.Fatalf("illegal row result wrong: %v", bad)
	}
	e.waitEmpty()

	// Stages: 20*12=240 -> emergence (12) and jointing (200) reached.
	pb := e.get(fmt.Sprintf("/api/v1/plots/%d", pid))
	stages := pb["stages"].([]any)
	find := func(name string) map[string]any {
		for _, s := range stages {
			sm := s.(map[string]any)
			if sm["stage"] == name {
				return sm
			}
		}
		t.Fatalf("stage %s missing", name)
		return nil
	}
	if find("emergence")["status"] != "reached" {
		t.Fatal("emergence not reached")
	}
	if find("jointing")["status"] != "reached" {
		t.Fatal("jointing not reached")
	}
	if find("tasseling")["status"] != "forecast" {
		t.Fatal("tasseling should be forecast")
	}

	// Daily curve: first day cum 12.
	days := e.get(fmt.Sprintf("/api/v1/plots/%d/days?from=2026-05-01&to=2026-05-01", pid))
	first := days["days"].([]any)[0].(map[string]any)
	if first["daily_gdd"].(float64) != 12 || first["cum_gdd"].(float64) != 12 {
		t.Fatalf("first day %v", first)
	}

	// At least one change event exists and is pullable.
	evs := e.get("/api/v1/events?after_id=0&limit=100")["events"].([]any)
	if len(evs) == 0 {
		t.Fatal("expected stage change events")
	}
	for _, ev := range evs {
		m := ev.(map[string]any)
		if m["reason"] == "" || m["stage"] == "" {
			t.Fatalf("event missing fields: %v", m)
		}
	}
}

func TestEndToEndValidationAndRebind(t *testing.T) {
	e := newE2E(t)

	// base >= ceiling rejected.
	st, b := e.post("/api/v1/varieties", map[string]any{
		"name": "bad", "base_temp": 30, "ceiling_temp": 20,
		"stage_require": map[string]float64{
			"emergence": 12, "jointing": 200, "tasseling": 500,
			"silking": 800, "maturity": 1200,
		},
	})
	if st != 400 {
		t.Fatalf("bad variety status %d body %v", st, b)
	}

	// Create two stations, a variety, plot.
	_, b1 := e.post("/api/v1/stations", map[string]any{
		"code": fmt.Sprintf("A-%d", time.Now().UnixNano()), "lat": 30, "lon": 120,
	})
	s1 := idOf(b1)
	_, b2 := e.post("/api/v1/stations", map[string]any{
		"code": fmt.Sprintf("B-%d", time.Now().UnixNano()), "lat": 30, "lon": 120,
	})
	s2 := idOf(b2)
	_, bv := e.post("/api/v1/varieties", map[string]any{
		"name": "ok", "base_temp": 10, "ceiling_temp": 35,
		"stage_require": map[string]float64{
			"emergence": 12, "jointing": 200, "tasseling": 500,
			"silking": 800, "maturity": 1200,
		},
	})
	vid := idOf(bv)
	_, bp := e.post("/api/v1/plots", map[string]any{
		"code":       fmt.Sprintf("P-%d", time.Now().UnixNano()),
		"variety_id": vid, "station_id": s1, "sowing_date": "2026-05-01",
	})
	pid := idOf(bp)

	// Rebind to nonexistent station -> 400.
	if st, _ := e.post(fmt.Sprintf("/api/v1/plots/%d/rebind", pid), map[string]any{
		"station_id": 9_999_999, "from": "2026-06-01",
	}); st != 400 {
		t.Fatalf("rebind to ghost station status %d", st)
	}

	// Rebind to real station -> 200 and binding visible.
	if st, _ := e.post(fmt.Sprintf("/api/v1/plots/%d/rebind", pid), map[string]any{
		"station_id": s2, "from": "2026-06-01",
	}); st != 200 {
		t.Fatalf("rebind status %d", st)
	}
	pb := e.get(fmt.Sprintf("/api/v1/plots/%d", pid))
	if len(pb["bindings"].([]any)) != 2 {
		t.Fatalf("expected 2 bindings, got %v", pb["bindings"])
	}

	// Query before sowing -> 400.
	resp, err := http.Get(e.srv.URL + fmt.Sprintf("/api/v1/plots/%d/days?from=2026-04-01", pid))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("pre-sowing query status %d", resp.StatusCode)
	}
}

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
	return newE2EAt(t, "2026-06-10")
}

func newE2EAt(t *testing.T, today string) *e2e {
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
	tday, _ := model.ParseDate(today)
	svc.Now = func() model.Date { return tday }
	svc.NowTS = func() time.Time {
		return tday.Time().Add(12 * time.Hour)
	}

	h := &api.Handler{Svc: svc, DB: db}
	srv := httptest.NewServer(api.NewRouter(h))
	t.Cleanup(srv.Close)

	e := &e2e{t: t, srv: srv, db: db, svc: svc}

	w := worker.New(db, 20*time.Millisecond)
	w.Today = func() model.Date { return tday }
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

// waitEmpty blocks until the worker has finished every task for the test run
// (pending AND in-flight processing). Counting only pending would return the
// instant a task is claimed, racing the recomputation itself.
func (e *e2e) waitEmpty() {
	ctx := context.Background()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := e.db.Pool.QueryRow(ctx,
			`SELECT count(*) FROM recompute_tasks
			 WHERE status IN ('pending','processing')
			   AND (run_after IS NULL OR run_after <= current_date)`).Scan(&n); err != nil {
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

// eventsList pulls events like e.get but tolerates the API encoding an empty
// list as JSON null (nil slice), returning an empty slice instead.
func (e *e2e) eventsList(afterID int64) []any {
	v := e.get(fmt.Sprintf("/api/v1/events?after_id=%d&limit=100", afterID))["events"]
	if v == nil {
		return nil
	}
	return v.([]any)
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

// reportedVariety creates the variety from the reported reproduction
// (base 10, ceiling 30; cumulative requirements 100/300/600/700/1200).
func (e *e2e) reportedVariety() int64 {
	_, b := e.post("/api/v1/varieties", map[string]any{
		"name": fmt.Sprintf("rep-%d", time.Now().UnixNano()), "base_temp": 10, "ceiling_temp": 30,
		"stage_require": map[string]float64{
			"emergence": 100, "jointing": 300, "tasseling": 600,
			"silking": 700, "maturity": 1200,
		},
	})
	return idOf(b)
}

func (e *e2e) mayJuneRows(sid int64) []map[string]any {
	rows := []map[string]any{}
	for day := model.NewDate(2026, 5, 1); !day.After(model.NewDate(2026, 6, 30)); day = day.AddDays(1) {
		rows = append(rows, map[string]any{
			"station_id": sid, "date": day.String(),
			"tmax": 28, "tmin": 14, "seq": 1,
		})
	}
	return rows
}

// runRepro performs the reported reproduction and returns the emitted events
// keyed by stage. waitBeforeBatch switches between the two rhythms: false =
// register then immediately batch; true = let the system settle first.
func (e *e2e) runRepro(t *testing.T, waitBeforeBatch bool) map[string]map[string]any {
	_, bs := e.post("/api/v1/stations", map[string]any{
		"code": fmt.Sprintf("R-%d", time.Now().UnixNano()), "lat": 30.0, "lon": 120.0,
	})
	sid := idOf(bs)
	vid := e.reportedVariety()
	_, bp := e.post("/api/v1/plots", map[string]any{
		"code":       fmt.Sprintf("RP-%d", time.Now().UnixNano()),
		"variety_id": vid, "station_id": sid, "sowing_date": "2026-05-01",
	})
	pid := idOf(bp)
	if waitBeforeBatch {
		e.waitEmpty()
		time.Sleep(100 * time.Millisecond)
	}
	if st, b := e.post("/api/v1/observations/batch", map[string]any{"rows": e.mayJuneRows(sid)}); st != 200 {
		t.Fatalf("batch %d %v", st, b)
	}
	e.waitEmpty()

	// Events are a global pull feed; keep only this plot's events so two
	// rhythms run back-to-back in one (un-cleaned) database do not collide.
	evs := e.eventsList(0)
	byStage := map[string]map[string]any{}
	for _, ev := range evs {
		m := ev.(map[string]any)
		if int64(m["plot_id"].(float64)) != pid {
			continue
		}
		byStage[m["stage"].(string)] = m
	}
	return byStage
}

// TestEventsSameRegardlessOfTiming is the reported bug: against a brand-new
// empty station, the May 1..Jun 30 batch arriving milliseconds after plot
// registration produced no events at all, while waiting a beat first produced
// one event per stage. Both rhythms must now yield the same five events, each
// with an empty old date and the reported dates.
func TestEventsSameRegardlessOfTiming(t *testing.T) {
	// Each rhythm gets an isolated database (its own e2e + cleanup) so the two
	// runs never share a worker or rows.
	var fast, slow map[string]map[string]any
	t.Run("immediate_batch", func(t *testing.T) {
		fast = newE2EAt(t, "2026-06-30").runRepro(t, false)
	})
	t.Run("delayed_batch", func(t *testing.T) {
		slow = newE2EAt(t, "2026-06-30").runRepro(t, true)
	})

	for _, stage := range []string{"emergence", "jointing", "tasseling", "silking", "maturity"} {
		f, ok := fast[stage]
		if !ok {
			t.Fatalf("fast rhythm: missing event for %s (bug: silent first prediction)", stage)
		}
		s, ok := slow[stage]
		if !ok {
			t.Fatalf("slow rhythm: missing event for %s", stage)
		}
		if f["old_date"] != nil {
			t.Fatalf("%s fast old_date must be null, got %v", stage, f["old_date"])
		}
		if s["old_date"] != nil {
			t.Fatalf("%s slow old_date must be null, got %v", stage, s["old_date"])
		}
		if f["new_date"] != s["new_date"] {
			t.Fatalf("%s new date depends on timing: %s vs %s", stage, f["new_date"], s["new_date"])
		}
	}
	want := map[string]string{
		"emergence": "2026-05-10", "jointing": "2026-05-28", "tasseling": "2026-06-24",
	}
	for stage, date := range want {
		if got := fast[stage]["new_date"]; got != date {
			t.Fatalf("%s = %v, want %s", stage, got, date)
		}
	}
}

// TestBaselinePredictableAtRegistrationEmitsNothing verifies that when dates
// are already computable at registration from a neighbor's climatology (the
// new plot's station has no history of its own), registration emits no event;
// real observations arriving later move the dates and each movement records
// old (registration-time) and new date.
func TestBaselinePredictableAtRegistrationEmitsNothing(t *testing.T) {
	e := newE2EAt(t, "2026-06-30")

	// Neighbor B carries five archive years of 26/14 (10 GDD/day).
	_, b := e.post("/api/v1/stations", map[string]any{
		"code": fmt.Sprintf("B-%d", time.Now().UnixNano()), "lat": 30.01, "lon": 120.01,
	})
	bid := idOf(b)
	archive := []map[string]any{}
	for yr := 2021; yr <= 2025; yr++ {
		for k := 0; k < 365; k++ {
			day := model.NewDate(yr, 1, 1).AddDays(k)
			archive = append(archive, map[string]any{
				"station_id": bid, "date": day.String(),
				"tmax": 26, "tmin": 14, "seq": 1,
			})
		}
	}
	if st, r := e.post("/api/v1/observations/batch", map[string]any{"rows": archive}); st != 200 {
		t.Fatalf("archive batch %d %v", st, r)
	}
	e.waitEmpty()

	// New station A without any data; the plot binds to A.
	_, a := e.post("/api/v1/stations", map[string]any{
		"code": fmt.Sprintf("A-%d", time.Now().UnixNano()), "lat": 30.0, "lon": 120.0,
	})
	aid := idOf(a)
	vid := e.reportedVariety()
	_, bp := e.post("/api/v1/plots", map[string]any{
		"code":       fmt.Sprintf("PA-%d", time.Now().UnixNano()),
		"variety_id": vid, "station_id": aid, "sowing_date": "2026-05-01",
	})
	pid := idOf(bp)
	e.waitEmpty()

	// Registration itself must not produce events even though every date is
	// already predictable from the neighbor.
	var regEvs []any
	for _, ev := range e.eventsList(0) {
		m := ev.(map[string]any)
		if int64(m["plot_id"].(float64)) == pid {
			regEvs = append(regEvs, m)
		}
	}
	if len(regEvs) != 0 {
		t.Fatalf("registration with predictable dates emitted events: %v", regEvs)
	}
	// ...and the dates are actually there, to be used as old dates later.
	pb := e.get(fmt.Sprintf("/api/v1/plots/%d", pid))
	baseline := map[string]string{}
	for _, s := range pb["stages"].([]any) {
		sm := s.(map[string]any)
		d, _ := sm["date"].(string)
		if d == "" {
			t.Fatalf("baseline stage %s should be predictable", sm["stage"])
		}
		baseline[sm["stage"].(string)] = d
	}

	// Real observations at A (28/14, 11 GDD/day): emergence stays 05-10, the
	// other four dates move earlier; each move records old and new.
	if st, r := e.post("/api/v1/observations/batch", map[string]any{"rows": e.mayJuneRows(aid)}); st != 200 {
		t.Fatalf("real batch %d %v", st, r)
	}
	e.waitEmpty()
	moved := map[string]map[string]any{}
	for _, ev := range e.eventsList(0) {
		m := ev.(map[string]any)
		if int64(m["plot_id"].(float64)) != pid {
			continue
		}
		stage := m["stage"].(string)
		if m["old_date"] != baseline[stage] {
			t.Fatalf("%s old_date %v must equal registration baseline %s", stage, m["old_date"], baseline[stage])
		}
		if m["new_date"] == nil || m["new_date"].(string) >= baseline[stage] {
			t.Fatalf("%s must move earlier: %s -> %v", stage, baseline[stage], m["new_date"])
		}
		moved[stage] = m
	}
	if len(moved) != 4 {
		t.Fatalf("want 4 moved stages (emergence unchanged on 05-10), got %d: %v", len(moved), moved)
	}
	if _, ok := moved["emergence"]; ok {
		t.Fatal("unchanged emergence date must not emit an event")
	}
}

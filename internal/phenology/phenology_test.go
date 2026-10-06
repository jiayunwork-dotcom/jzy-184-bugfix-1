package phenology

import (
	"testing"
	"time"

	"agriheat/internal/model"
)

func pdate(y, m, d int) model.Date { return model.NewDate(y, time.Month(m), d) }

func reqs() map[model.Stage]float64 {
	return map[model.Stage]float64{
		model.StageEmergence: 10,
		model.StageJointing:  30,
		model.StageTasseling: 60,
		model.StageSilking:   90,
		model.StageMaturity:  120,
	}
}

func TestStageCrossingAndStatus(t *testing.T) {
	// 5 days * 10 GDD past = cum 50; then forecast 10/day reaches all.
	sowing := pdate(2026, 5, 1)
	asOf := pdate(2026, 5, 5)
	rows := make([]DayRow, 0, 20)
	for i := 0; i < 20; i++ {
		day := sowing.AddDays(i)
		src := model.SourceObs
		if day.After(asOf) {
			src = model.SourceClimate
		}
		rows = append(rows, DayRow{Date: day, GDD: 10, Source: src})
	}
	res := Stages(rows, asOf, reqs())
	by := map[model.Stage]StageResult{}
	for _, r := range res {
		by[r.Stage] = r
	}
	// emergence cum 10 on the sowing day itself -> reached 2026-05-01.
	if by[model.StageEmergence].Date != pdate(2026, 5, 1) || by[model.StageEmergence].Status != StatusReached {
		t.Fatalf("emergence %+v", by[model.StageEmergence])
	}
	// jointing cum 30 at index 2 (2026-05-03) -> reached.
	if by[model.StageJointing].Date != pdate(2026, 5, 3) {
		t.Fatalf("jointing %+v", by[model.StageJointing])
	}
	// tasseling cum 60 at index 5 (2026-05-06) -> forecast.
	if by[model.StageTasseling].Date != pdate(2026, 5, 6) || by[model.StageTasseling].Status != StatusForecast {
		t.Fatalf("tasseling %+v", by[model.StageTasseling])
	}
	if by[model.StageMaturity].Date != pdate(2026, 5, 12) {
		t.Fatalf("maturity %+v", by[model.StageMaturity])
	}
}

func TestUnreachedStagesAbsent(t *testing.T) {
	rows := []DayRow{{Date: pdate(2026, 5, 1), GDD: 1, Source: model.SourceObs}}
	res := Stages(rows, pdate(2026, 5, 1), reqs())
	if len(res) != 0 {
		t.Fatalf("no threshold crossed, got %+v", res)
	}
}

func TestCumAt(t *testing.T) {
	rows := []DayRow{{GDD: 5}, {GDD: 7}, {GDD: 0}, {GDD: 3}}
	cum := CumAt(rows)
	want := []float64{5, 12, 12, 15}
	for i := range want {
		if cum[i] != want[i] {
			t.Fatalf("cum[%d]=%v want %v", i, cum[i], want[i])
		}
	}
}

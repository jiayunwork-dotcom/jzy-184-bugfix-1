package events

import (
	"testing"
	"time"

	"agriheat/internal/model"
	"agriheat/internal/phenology"
)

func date(y, m, d int) model.Date { return model.NewDate(y, time.Month(m), d) }

func TestDiffMoveAndUnknown(t *testing.T) {
	drafts := []Draft{
		{Stage: model.StageEmergence, Status: phenology.StatusReached, Date: date(2026, 5, 3)},
		{Stage: model.StageJointing, Status: phenology.StatusForecast, Date: date(2026, 6, 9)},
		{Stage: model.StageTasseling, Status: phenology.StatusForecast, Date: date(2026, 7, 5)},
	}
	oldDate := map[model.Stage]model.Date{
		model.StageEmergence: date(2026, 5, 2),
		model.StageJointing:  date(2026, 6, 12),
	}
	oldKnown := map[model.Stage]bool{
		model.StageEmergence: true,
		model.StageJointing:  true,
	}
	changes := Diff(7, drafts, oldDate, oldKnown, "station 1 2026-05-01 corrected")
	if len(changes) != 3 {
		t.Fatalf("want 3 changes, got %d: %+v", len(changes), changes)
	}
	byStage := map[model.Stage]Change{}
	for _, c := range changes {
		byStage[c.Stage] = c
	}
	e := byStage[model.StageEmergence]
	if e.OldDate == nil || !e.OldDate.Equal(date(2026, 5, 2)) {
		t.Fatalf("emergence old %v", e.OldDate)
	}
	if e.NewDate == nil || !e.NewDate.Equal(date(2026, 5, 3)) {
		t.Fatalf("emergence new %v", e.NewDate)
	}
	// Forecast moved earlier as well — still a change event.
	j := byStage[model.StageJointing]
	if !j.NewDate.Equal(date(2026, 6, 9)) || !j.OldDate.Equal(date(2026, 6, 12)) {
		t.Fatalf("jointing %+v", j)
	}
	// Unknown -> known.
	tas := byStage[model.StageTasseling]
	if tas.OldDate != nil || tas.NewDate == nil {
		t.Fatalf("tasseling %+v", tas)
	}
}

func TestDiffNoChangeNoEvent(t *testing.T) {
	day := date(2026, 5, 3)
	drafts := []Draft{{Stage: model.StageEmergence, Status: phenology.StatusReached, Date: day}}
	oldDate := map[model.Stage]model.Date{model.StageEmergence: day}
	oldKnown := map[model.Stage]bool{model.StageEmergence: true}
	if cs := Diff(1, drafts, oldDate, oldKnown, "r"); len(cs) != 0 {
		t.Fatalf("identical dates must not emit events: %+v", cs)
	}
	// Both unknown also emits nothing.
	if cs := Diff(1, []Draft{{Stage: model.StageEmergence}}, nil, nil, "r"); len(cs) != 0 {
		t.Fatalf("double-unknown must not emit: %+v", cs)
	}
}

func TestDiffKnownToUnknown(t *testing.T) {
	day := date(2026, 5, 3)
	drafts := []Draft{{Stage: model.StageEmergence}} // projection lost
	oldDate := map[model.Stage]model.Date{model.StageEmergence: day}
	oldKnown := map[model.Stage]bool{model.StageEmergence: true}
	cs := Diff(1, drafts, oldDate, oldKnown, "r")
	if len(cs) != 1 || cs[0].NewDate != nil || cs[0].OldDate == nil {
		t.Fatalf("known->unknown change wrong: %+v", cs)
	}
}

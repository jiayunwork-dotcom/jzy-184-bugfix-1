package validate

import (
	"testing"
	"time"

	"agriheat/internal/model"
)

func TestCheckTemp(t *testing.T) {
	if errs := CheckTemp(20, 25); len(errs) == 0 {
		t.Fatal("tmin > tmax must be rejected")
	} else if errs[0].Field != "tmin" {
		t.Fatalf("want tmin field, got %s", errs[0].Field)
	}
	if errs := CheckTemp(500, 10); len(errs) == 0 {
		t.Fatal("implausible tmax must be rejected")
	}
	if errs := CheckTemp(-100, -120); len(errs) == 0 {
		t.Fatal("implausible tmin must be rejected")
	}
	if errs := CheckTemp(30, -10); len(errs) != 0 {
		t.Fatalf("valid extreme range rejected: %v", errs)
	}
	// Both errors reported for one row (batch lists each reason).
	errs := CheckTemp(500, 600)
	if len(errs) < 2 {
		t.Fatalf("expected multiple reasons, got %v", errs)
	}
}

func TestCheckVarietyParams(t *testing.T) {
	req := map[model.Stage]float64{
		model.StageEmergence: 50,
		model.StageJointing:  200,
		model.StageTasseling: 500,
		model.StageSilking:   800,
		model.StageMaturity:  1200,
	}
	if errs := CheckVarietyParams(10, 30, req); len(errs) != 0 {
		t.Fatalf("valid variety rejected: %v", errs)
	}
	if errs := CheckVarietyParams(30, 10, req); len(errs) == 0 {
		t.Fatal("base >= ceiling must be rejected")
	}
	if errs := CheckVarietyParams(10, 10, req); len(errs) == 0 {
		t.Fatal("base == ceiling must be rejected")
	}
	bad := map[model.Stage]float64{
		model.StageEmergence: 50,
		model.StageJointing:  200,
		model.StageTasseling: 150, // decreases
		model.StageSilking:   800,
		model.StageMaturity:  1200,
	}
	if errs := CheckVarietyParams(10, 30, bad); len(errs) == 0 {
		t.Fatal("non-increasing stage requirements must be rejected")
	}
	eq := map[model.Stage]float64{
		model.StageEmergence: 50,
		model.StageJointing:  50, // equal
		model.StageTasseling: 500,
		model.StageSilking:   800,
		model.StageMaturity:  1200,
	}
	if errs := CheckVarietyParams(10, 30, eq); len(errs) == 0 {
		t.Fatal("equal adjacent requirements must be rejected")
	}
	missing := map[model.Stage]float64{model.StageEmergence: 50}
	if errs := CheckVarietyParams(10, 30, missing); len(errs) == 0 {
		t.Fatal("missing stages must be rejected")
	}
	nonpos := map[model.Stage]float64{
		model.StageEmergence: 0,
		model.StageJointing:  200,
		model.StageTasseling: 500,
		model.StageSilking:   800,
		model.StageMaturity:  1200,
	}
	if errs := CheckVarietyParams(10, 30, nonpos); len(errs) == 0 {
		t.Fatal("zero requirement must be rejected")
	}
}

func TestCheckSowingDate(t *testing.T) {
	s := model.NewDate(2026, time.May, 1)
	if err := CheckSowingDate(s, model.NewDate(2026, time.April, 30)); err == nil {
		t.Fatal("query before sowing must fail")
	}
	if err := CheckSowingDate(s, s); err != nil {
		t.Fatalf("same day must pass: %v", err)
	}
	if err := CheckSowingDate(s, model.NewDate(2026, time.May, 2)); err != nil {
		t.Fatalf("later query must pass: %v", err)
	}
}

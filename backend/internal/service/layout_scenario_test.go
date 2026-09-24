package service

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"datacenter-thermal-capacity-planner/backend/internal/audit"
	"datacenter-thermal-capacity-planner/backend/internal/constants"
	"datacenter-thermal-capacity-planner/backend/internal/database"
	"datacenter-thermal-capacity-planner/backend/internal/dto"
	"datacenter-thermal-capacity-planner/backend/internal/model"
	"datacenter-thermal-capacity-planner/backend/internal/planner"
	"datacenter-thermal-capacity-planner/backend/internal/repository"
	"datacenter-thermal-capacity-planner/backend/internal/web"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

var testDBCounter uint64

func gormOpenSQLite() (*gorm.DB, error) {
	// A unique shared-cache in-memory DSN per test keeps isolated schemas;
	// a single connection keeps the in-memory database alive for the test.
	dsn := fmt.Sprintf("file:svctest%d?mode=memory&cache=shared", atomic.AddUint64(&testDBCounter, 1))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, err
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	return db, nil
}

func newStaleGuardService(t *testing.T) (*LayoutScenarioService, *gorm.DB) {
	t.Helper()
	db, err := openTestDB()
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	auditRepo := audit.NewRepository(db)
	zoneRepo := repository.NewThermalZoneRepository(db, auditRepo)
	rackRepo := repository.NewRackRepository(db, auditRepo)
	loadRepo := repository.NewEquipmentLoadRepository(db, auditRepo)
	scenarioRepo := repository.NewLayoutScenarioRepository(db, auditRepo)
	svc := NewLayoutScenarioService(scenarioRepo, zoneRepo, rackRepo, loadRepo, planner.NewEngine(500))
	return svc, db
}

func openTestDB() (*gorm.DB, error) {
	db, err := gormOpenSQLite()
	if err != nil {
		return nil, err
	}
	if err := database.Migrate(db); err != nil {
		return nil, err
	}
	return db, nil
}

func seedStaleInputs(t *testing.T, db *gorm.DB) (zone model.ThermalZone, rack model.Rack, load model.EquipmentLoad) {
	t.Helper()
	zone = model.ThermalZone{
		ZoneCode: "TZ-T", Name: "Test aisle", CoolingCapacityKW: 100,
		SupplyTempC: 18, MaxReturnTempC: 32, AdjacencyJSON: `{}`, ZoneStatus: "active",
	}
	if err := db.Create(&zone).Error; err != nil {
		t.Fatalf("create zone: %v", err)
	}
	rack = model.Rack{
		ZoneID: zone.ID, RackCode: "T-01", RowIndex: 0, ColumnIndex: 0,
		PowerLimitKW: 30, AirflowLimitCFM: 9000, RackUnits: 42,
		RackStatus: constants.RackAvailable, Version: 1,
	}
	if err := db.Create(&rack).Error; err != nil {
		t.Fatalf("create rack: %v", err)
	}
	load = model.EquipmentLoad{
		Name: "test-node", PowerKW: 5, HeatKW: 4.5, AirflowCFM: 1000, RackUnits: 5,
		RedundancyGroup: "G1", PreferredZoneID: &zone.ID, LoadStatus: "ready",
	}
	if err := db.Create(&load).Error; err != nil {
		t.Fatalf("create load: %v", err)
	}
	return zone, rack, load
}

func plannerActor() audit.Entry {
	return audit.Entry{RequestID: "req-test", ActorID: 1, ActorUsername: "planner"}
}

func TestApproveRejectedWhenRackChangedThenAcceptedAfterReEvaluation(t *testing.T) {
	ctx := context.Background()
	svc, db := newStaleGuardService(t)
	_, rack, load := seedStaleInputs(t, db)

	created, err := svc.Create(ctx, dto.CreateLayoutScenarioRequest{Name: "stale guard scenario", LoadIDs: []uint{load.ID}}, plannerActor())
	if err != nil {
		t.Fatalf("create scenario: %v", err)
	}
	evaluated, err := svc.Evaluate(ctx, created.ID, created.Version, plannerActor())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if evaluated.ScenarioStatus != constants.ScenarioPendingReview {
		t.Fatalf("status = %s, want pending_review", evaluated.ScenarioStatus)
	}
	if evaluated.HasCriticalViolation {
		t.Fatalf("test inputs must not produce critical violations: %+v", evaluated.Violations)
	}
	if !evaluated.InputsFresh {
		t.Fatal("freshly evaluated scenario must report inputs_fresh")
	}

	// Planner edits the rack after the review snapshot was taken.
	if err := db.Model(&model.Rack{}).Where("id = ?", rack.ID).
		Update("power_limit_kw", 12.0).Error; err != nil {
		t.Fatalf("change rack: %v", err)
	}

	stale, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get scenario: %v", err)
	}
	if !stale.InputsChangedSinceEval || stale.InputsFresh {
		t.Fatalf("scenario should be flagged stale: fresh=%v changes=%v", stale.InputsFresh, stale.InputChanges)
	}
	if len(stale.InputChanges) != 1 || stale.InputChanges[0].EntityType != "rack" {
		t.Fatalf("expected one rack change, got %+v", stale.InputChanges)
	}
	if stale.InputChanges[0].EntityCode != "T-01" {
		t.Fatalf("change must identify the rack code, got %q", stale.InputChanges[0].EntityCode)
	}

	_, err = svc.Transition(ctx, created.ID, dto.TransitionScenarioRequest{
		TargetStatus: constants.ScenarioApproved, Version: evaluated.Version,
	}, audit.Entry{RequestID: "req-test", ActorID: 2, ActorUsername: "reviewer"})
	if err == nil {
		t.Fatal("approval must be rejected when inputs changed")
	}
	appErr, ok := err.(*web.AppError)
	if !ok || appErr.Code != "SCENARIO_INPUTS_CHANGED" {
		t.Fatalf("error = %v, want SCENARIO_INPUTS_CHANGED", err)
	}

	// Reviewer sends it back, planner re-evaluates against current inputs.
	sentBack, err := svc.Transition(ctx, created.ID, dto.TransitionScenarioRequest{
		TargetStatus: constants.ScenarioDraft, Version: evaluated.Version,
	}, audit.Entry{RequestID: "req-test", ActorID: 2, ActorUsername: "reviewer"})
	if err != nil {
		t.Fatalf("send back to draft: %v", err)
	}
	reevaluated, err := svc.Evaluate(ctx, created.ID, sentBack.Version, plannerActor())
	if err != nil {
		t.Fatalf("re-evaluate: %v", err)
	}
	if !reevaluated.InputsFresh || reevaluated.InputsChangedSinceEval {
		t.Fatalf("re-evaluated scenario must be fresh, changes=%v", reevaluated.InputChanges)
	}
	approved, err := svc.Transition(ctx, created.ID, dto.TransitionScenarioRequest{
		TargetStatus: constants.ScenarioApproved, Version: reevaluated.Version,
	}, audit.Entry{RequestID: "req-test", ActorID: 2, ActorUsername: "reviewer"})
	if err != nil {
		t.Fatalf("approve after re-evaluation: %v", err)
	}
	if approved.ScenarioStatus != constants.ScenarioApproved {
		t.Fatalf("status = %s, want approved", approved.ScenarioStatus)
	}

	// The stale result remains available for comparison against the new one.
	comparison, err := svc.Compare(ctx, created.ID, created.ID)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if comparison.Left.ID != created.ID || comparison.Right.ID != created.ID {
		t.Fatal("old evaluation must stay comparable")
	}
}

func TestApproveRejectedWhenSelectedLoadChanged(t *testing.T) {
	ctx := context.Background()
	svc, db := newStaleGuardService(t)
	_, _, load := seedStaleInputs(t, db)

	created, err := svc.Create(ctx, dto.CreateLayoutScenarioRequest{Name: "load drift scenario", LoadIDs: []uint{load.ID}}, plannerActor())
	if err != nil {
		t.Fatalf("create scenario: %v", err)
	}
	evaluated, err := svc.Evaluate(ctx, created.ID, created.Version, plannerActor())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if err := db.Model(&model.EquipmentLoad{}).Where("id = ?", load.ID).
		Update("power_kw", 25.0).Error; err != nil {
		t.Fatalf("change load: %v", err)
	}

	listed, _, err := svc.List(ctx, "", "", 1, 50)
	if err != nil {
		t.Fatalf("list scenarios: %v", err)
	}
	if len(listed) != 1 || !listed[0].InputsChangedSinceEval {
		t.Fatalf("list endpoint must flag the stale scenario, got %+v", listed)
	}

	_, err = svc.Transition(ctx, created.ID, dto.TransitionScenarioRequest{
		TargetStatus: constants.ScenarioApproved, Version: evaluated.Version,
	}, audit.Entry{RequestID: "req-test", ActorID: 2, ActorUsername: "reviewer"})
	if err == nil {
		t.Fatal("approval must be rejected when the selected load changed")
	}
	appErr, ok := err.(*web.AppError)
	if !ok || appErr.Code != "SCENARIO_INPUTS_CHANGED" {
		t.Fatalf("error = %v, want SCENARIO_INPUTS_CHANGED", err)
	}
}

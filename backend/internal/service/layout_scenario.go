package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"datacenter-thermal-capacity-planner/backend/internal/audit"
	"datacenter-thermal-capacity-planner/backend/internal/constants"
	"datacenter-thermal-capacity-planner/backend/internal/dto"
	"datacenter-thermal-capacity-planner/backend/internal/model"
	"datacenter-thermal-capacity-planner/backend/internal/planner"
	"datacenter-thermal-capacity-planner/backend/internal/repository"
	"datacenter-thermal-capacity-planner/backend/internal/web"
)

type LayoutScenarioService struct {
	scenarios *repository.LayoutScenarioRepository
	zones     *repository.ThermalZoneRepository
	racks     *repository.RackRepository
	loads     *repository.EquipmentLoadRepository
	engine    *planner.Engine
}

type scenarioInputSnapshot struct {
	LoadIDs          []uint `json:"load_ids"`
	AlgorithmVersion string `json:"algorithm_version"`
}

func NewLayoutScenarioService(scenarios *repository.LayoutScenarioRepository, zones *repository.ThermalZoneRepository, racks *repository.RackRepository, loads *repository.EquipmentLoadRepository, engine *planner.Engine) *LayoutScenarioService {
	return &LayoutScenarioService{scenarios: scenarios, zones: zones, racks: racks, loads: loads, engine: engine}
}

func (s *LayoutScenarioService) List(ctx context.Context, search, status string, page, size int) ([]dto.ScenarioResponse, int64, error) {
	items, total, err := s.scenarios.List(ctx, search, status, page, size)
	if err != nil {
		return nil, 0, err
	}
	zones, racks, loadMap, err := s.currentInputs(ctx)
	if err != nil {
		return nil, 0, err
	}
	responses := make([]dto.ScenarioResponse, 0, len(items))
	for _, item := range items {
		responses = append(responses, s.decorate(item, zones, racks, loadMap))
	}
	return responses, total, nil
}

func (s *LayoutScenarioService) Get(ctx context.Context, id uint) (dto.ScenarioResponse, error) {
	item, err := s.scenarios.Get(ctx, id)
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	zones, racks, loadMap, err := s.currentInputs(ctx)
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	return s.decorate(item, zones, racks, loadMap), nil
}

// currentInputs loads the live racks, zones and selected loads used to detect
// whether an evaluated scenario has stale inputs.
func (s *LayoutScenarioService) currentInputs(ctx context.Context) ([]model.ThermalZone, []model.Rack, map[uint]model.EquipmentLoad, error) {
	zones, err := s.zones.All(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	racks, err := s.racks.All(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	allLoads, err := s.loads.All(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	loadMap := make(map[uint]model.EquipmentLoad, len(allLoads))
	for _, load := range allLoads {
		loadMap[load.ID] = load
	}
	return zones, racks, loadMap, nil
}

// decorate decodes a scenario and attaches its input freshness report.
func (s *LayoutScenarioService) decorate(item model.LayoutScenario, zones []model.ThermalZone, racks []model.Rack, loadMap map[uint]model.EquipmentLoad) dto.ScenarioResponse {
	response := dto.DecodeScenario(item)
	if snapshot, evaluated := planner.DecodeSnapshot(item.InputSnapshotJSON); evaluated {
		loads := make([]model.EquipmentLoad, 0, len(snapshot.Loads))
		for _, snapLoad := range snapshot.Loads {
			if current, exists := loadMap[snapLoad.ID]; exists {
				loads = append(loads, current)
			}
		}
		changes := planner.DiffInputs(snapshot, zones, racks, loads)
		freshness := dto.InputFreshness{Evaluated: true, Stale: len(changes) > 0, ChangeCount: len(changes), Changes: changes}
		if freshness.Stale {
			freshness.WarningMessage = staleWarning(changes)
		}
		response.InputFreshness = &freshness
	}
	return response
}

func staleWarning(changes []dto.InputChange) string {
	zones, racks, loads := 0, 0, 0
	for _, change := range changes {
		switch change.EntityType {
		case "thermal_zone":
			zones++
		case "rack":
			racks++
		case "equipment_load":
			loads++
		}
	}
	parts := []string{}
	if zones > 0 {
		parts = append(parts, fmt.Sprintf("%d thermal zone field(s)", zones))
	}
	if racks > 0 {
		parts = append(parts, fmt.Sprintf("%d rack field(s)", racks))
	}
	if loads > 0 {
		parts = append(parts, fmt.Sprintf("%d equipment load field(s)", loads))
	}
	return "inputs changed since evaluation (" + strings.Join(parts, ", ") + "); re-evaluate before approval"
}

func (s *LayoutScenarioService) Create(ctx context.Context, req dto.CreateLayoutScenarioRequest, actor audit.Entry) (dto.ScenarioResponse, error) {
	if err := req.ValidateBusiness(); err != nil {
		return dto.ScenarioResponse{}, web.Unprocessable("INVALID_SCENARIO", err.Error(), err)
	}
	loads, err := s.loads.FindByIDs(ctx, req.LoadIDs)
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	for _, load := range loads {
		if !load.IsPlannable() {
			return dto.ScenarioResponse{}, web.Unprocessable("LOAD_NOT_READY", fmt.Sprintf("load %d is not ready for planning", load.ID), nil)
		}
	}
	snapshot, err := json.Marshal(scenarioInputSnapshot{LoadIDs: req.LoadIDs, AlgorithmVersion: planner.AlgorithmVersion})
	if err != nil {
		return dto.ScenarioResponse{}, web.Internal(fmt.Errorf("encode scenario input: %w", err))
	}
	item := model.LayoutScenario{
		Name: strings.TrimSpace(req.Name), ScenarioStatus: constants.ScenarioDraft,
		RackAssignmentsJSON: "[]", InputSnapshotJSON: string(snapshot), ZoneResultsJSON: "[]",
		ConstraintViolationsJSON: "[]", AlgorithmVersion: planner.AlgorithmVersion,
		Version: 1, CreatedBy: actor.ActorID,
	}
	actor.Action = "layout_scenario.create"
	actor.EntityType = "layout_scenario"
	actor.AfterSummary = fmt.Sprintf("draft loads=%v algorithm=%s", req.LoadIDs, planner.AlgorithmVersion)
	if err := s.scenarios.Create(ctx, &item, actor); err != nil {
		return dto.ScenarioResponse{}, err
	}
	return dto.DecodeScenario(item), nil
}

func (s *LayoutScenarioService) Evaluate(ctx context.Context, id, version uint, actor audit.Entry) (dto.ScenarioResponse, error) {
	current, err := s.scenarios.Get(ctx, id)
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	var input scenarioInputSnapshot
	if err := json.Unmarshal([]byte(current.InputSnapshotJSON), &input); err != nil || len(input.LoadIDs) == 0 {
		return dto.ScenarioResponse{}, web.Unprocessable("INVALID_SCENARIO_SNAPSHOT", "scenario input snapshot cannot be evaluated", err)
	}
	zones, err := s.zones.All(ctx)
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	racks, err := s.racks.All(ctx)
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	loads, err := s.loads.FindByIDs(ctx, input.LoadIDs)
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	actor.Action = "layout_scenario.evaluate.start"
	actor.EntityType = "layout_scenario"
	evaluating, err := s.scenarios.BeginEvaluation(ctx, id, version, actor)
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	result := s.engine.Evaluate(zones, racks, loads)
	assignments, err := json.Marshal(result.Assignments)
	if err != nil {
		return dto.ScenarioResponse{}, web.Internal(fmt.Errorf("encode assignments: %w", err))
	}
	zoneResults, err := json.Marshal(result.ZoneResults)
	if err != nil {
		return dto.ScenarioResponse{}, web.Internal(fmt.Errorf("encode zone results: %w", err))
	}
	violations, err := json.Marshal(result.Violations)
	if err != nil {
		return dto.ScenarioResponse{}, web.Internal(fmt.Errorf("encode violations: %w", err))
	}
	fullSnapshot, err := json.Marshal(struct {
		LoadIDs          []uint                `json:"load_ids"`
		AlgorithmVersion string                `json:"algorithm_version"`
		Zones            []model.ThermalZone   `json:"zones"`
		Racks            []model.Rack          `json:"racks"`
		Loads            []model.EquipmentLoad `json:"loads"`
	}{LoadIDs: input.LoadIDs, AlgorithmVersion: planner.AlgorithmVersion, Zones: zones, Racks: racks, Loads: loads})
	if err != nil {
		return dto.ScenarioResponse{}, web.Internal(fmt.Errorf("encode evaluation snapshot: %w", err))
	}
	update := repository.EvaluationUpdate{
		AssignmentsJSON: string(assignments), SnapshotJSON: string(fullSnapshot),
		ZoneResultsJSON: string(zoneResults), ViolationsJSON: string(violations),
		TotalPowerKW: result.TotalPower, PeakTempC: result.PeakTemp, Score: result.Score,
	}
	actor.Action = "layout_scenario.evaluate.finish"
	actor.BeforeSummary = fmt.Sprintf("algorithm=%s input_loads=%d", planner.AlgorithmVersion, len(loads))
	actor.AfterSummary = fmt.Sprintf("score=%.2f assignments=%d violations=%d", result.Score, len(result.Assignments), len(result.Violations))
	if err := s.scenarios.FinishEvaluation(ctx, evaluating, update, actor); err != nil {
		return dto.ScenarioResponse{}, err
	}
	return s.Get(ctx, id)
}

func (s *LayoutScenarioService) Transition(ctx context.Context, id uint, req dto.TransitionScenarioRequest, actor audit.Entry) (dto.ScenarioResponse, error) {
	current, err := s.scenarios.Get(ctx, id)
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	if current.Version != req.Version {
		return dto.ScenarioResponse{}, web.Conflict("SCENARIO_VERSION_CONFLICT", "scenario was changed by another user", nil)
	}
	if !constants.ValidScenarioStatus(req.TargetStatus) || !constants.CanTransitionScenario(current.ScenarioStatus, req.TargetStatus) {
		return dto.ScenarioResponse{}, web.Unprocessable("INVALID_SCENARIO_TRANSITION", fmt.Sprintf("cannot transition scenario from %s to %s", current.ScenarioStatus, req.TargetStatus), nil)
	}
	decoded := dto.DecodeScenario(current)
	if req.TargetStatus == constants.ScenarioApproved {
		if decoded.HasCriticalViolation {
			return dto.ScenarioResponse{}, web.Unprocessable("CRITICAL_VIOLATIONS", "scenario cannot be approved while critical violations remain", nil)
		}
		// Re-verify the evaluation snapshot against live inputs at approval
		// time so a reviewer cannot approve a layout based on stale racks,
		// thermal zones or equipment loads.
		snapshot, evaluated := planner.DecodeSnapshot(current.InputSnapshotJSON)
		if !evaluated {
			return dto.ScenarioResponse{}, web.Unprocessable("SCENARIO_NOT_EVALUATED", "scenario must be evaluated before approval", nil)
		}
		zones, err := s.zones.All(ctx)
		if err != nil {
			return dto.ScenarioResponse{}, err
		}
		racks, err := s.racks.All(ctx)
		if err != nil {
			return dto.ScenarioResponse{}, err
		}
		allLoads, err := s.loads.All(ctx)
		if err != nil {
			return dto.ScenarioResponse{}, err
		}
		loadByID := make(map[uint]model.EquipmentLoad, len(allLoads))
		for _, load := range allLoads {
			loadByID[load.ID] = load
		}
		liveLoads := make([]model.EquipmentLoad, 0, len(snapshot.Loads))
		for _, snapLoad := range snapshot.Loads {
			if currentLoad, exists := loadByID[snapLoad.ID]; exists {
				liveLoads = append(liveLoads, currentLoad)
			}
		}
		changes := planner.DiffInputs(snapshot, zones, racks, liveLoads)
		if len(changes) > 0 {
			return dto.ScenarioResponse{}, web.Conflict("SCENARIO_INPUTS_CHANGED", staleWarning(changes)+"; send the scenario back to draft and re-evaluate", nil)
		}
	}
	actor.Action = "layout_scenario.transition"
	if req.TargetStatus == constants.ScenarioApproved {
		actor.Action = "layout_scenario.approve"
	}
	actor.EntityType = "layout_scenario"
	actor.BeforeSummary = string(current.ScenarioStatus)
	actor.AfterSummary = fmt.Sprintf("%s reason=%s", req.TargetStatus, strings.TrimSpace(req.Reason))
	if err := s.scenarios.Transition(ctx, current, req.TargetStatus, actor.ActorID, actor); err != nil {
		return dto.ScenarioResponse{}, err
	}
	return s.Get(ctx, id)
}

func (s *LayoutScenarioService) Compare(ctx context.Context, leftID, rightID uint) (dto.ScenarioComparison, error) {
	left, err := s.Get(ctx, leftID)
	if err != nil {
		return dto.ScenarioComparison{}, err
	}
	right, err := s.Get(ctx, rightID)
	if err != nil {
		return dto.ScenarioComparison{}, err
	}
	summary := []string{
		fmt.Sprintf("Score changed by %.2f points", right.Score-left.Score),
		fmt.Sprintf("Peak return temperature changed by %.2f C", right.PeakTempC-left.PeakTempC),
		fmt.Sprintf("Critical flag changed from %t to %t", left.HasCriticalViolation, right.HasCriticalViolation),
	}
	return dto.ScenarioComparison{
		Left: left, Right: right, ScoreDelta: right.Score - left.Score,
		PowerDeltaKW:  right.TotalPowerKW - left.TotalPowerKW,
		PeakTempDelta: right.PeakTempC - left.PeakTempC, Summary: summary,
	}, nil
}

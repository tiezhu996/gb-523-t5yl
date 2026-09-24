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

func NewLayoutScenarioService(scenarios *repository.LayoutScenarioRepository, zones *repository.ThermalZoneRepository, racks *repository.RackRepository, loads *repository.EquipmentLoadRepository, engine *planner.Engine) *LayoutScenarioService {
	return &LayoutScenarioService{scenarios: scenarios, zones: zones, racks: racks, loads: loads, engine: engine}
}

func (s *LayoutScenarioService) List(ctx context.Context, search, status string, page, size int) ([]dto.ScenarioResponse, int64, error) {
	items, total, err := s.scenarios.List(ctx, search, status, page, size)
	if err != nil {
		return nil, 0, err
	}
	zones, err := s.zones.All(ctx)
	if err != nil {
		return nil, 0, err
	}
	racks, err := s.racks.All(ctx)
	if err != nil {
		return nil, 0, err
	}
	responses := make([]dto.ScenarioResponse, 0, len(items))
	for _, item := range items {
		response := dto.DecodeScenario(item)
		if err := s.attachFreshness(ctx, item, &response, zones, racks); err != nil {
			return nil, 0, err
		}
		responses = append(responses, response)
	}
	return responses, total, nil
}

func (s *LayoutScenarioService) Get(ctx context.Context, id uint) (dto.ScenarioResponse, error) {
	item, err := s.scenarios.Get(ctx, id)
	if err != nil {
		return dto.ScenarioResponse{}, err
	}
	response := dto.DecodeScenario(item)
	if err := s.attachFreshness(ctx, item, &response, nil, nil); err != nil {
		return dto.ScenarioResponse{}, err
	}
	return response, nil
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
	rawSnapshot, err := planner.Encode(planner.Inputs{
		Kind: planner.KindDraft, LoadIDs: req.LoadIDs, AlgorithmVersion: planner.AlgorithmVersion,
	})
	if err != nil {
		return dto.ScenarioResponse{}, web.Internal(fmt.Errorf("encode scenario input: %w", err))
	}
	item := model.LayoutScenario{
		Name: strings.TrimSpace(req.Name), ScenarioStatus: constants.ScenarioDraft,
		RackAssignmentsJSON: "[]", InputSnapshotJSON: rawSnapshot, ZoneResultsJSON: "[]",
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
	input, err := planner.Decode(current.InputSnapshotJSON)
	if err != nil || len(input.LoadIDs) == 0 {
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
	assignments, err := encodeJSON(result.Assignments)
	if err != nil {
		return dto.ScenarioResponse{}, web.Internal(fmt.Errorf("encode assignments: %w", err))
	}
	zoneResults, err := encodeJSON(result.ZoneResults)
	if err != nil {
		return dto.ScenarioResponse{}, web.Internal(fmt.Errorf("encode zone results: %w", err))
	}
	violations, err := encodeJSON(result.Violations)
	if err != nil {
		return dto.ScenarioResponse{}, web.Internal(fmt.Errorf("encode violations: %w", err))
	}
	fullSnapshot, err := planner.Encode(planner.Inputs{
		Kind: planner.KindEvaluation, LoadIDs: input.LoadIDs, AlgorithmVersion: planner.AlgorithmVersion,
		Zones: zones, Racks: racks, Loads: loads,
	})
	if err != nil {
		return dto.ScenarioResponse{}, web.Internal(fmt.Errorf("encode evaluation snapshot: %w", err))
	}
	update := repository.EvaluationUpdate{
		AssignmentsJSON: assignments, SnapshotJSON: fullSnapshot,
		ZoneResultsJSON: zoneResults, ViolationsJSON: violations,
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
	if req.TargetStatus == constants.ScenarioApproved && decoded.HasCriticalViolation {
		return dto.ScenarioResponse{}, web.Unprocessable("CRITICAL_VIOLATIONS", "scenario cannot be approved while critical violations remain", nil)
	}

	// Approval must re-check the evaluation snapshot against current inputs
	// inside the same transaction so a change made between this check and the
	// status update cannot slip through.
	var verify repository.TransitionGuard
	if req.TargetStatus == constants.ScenarioApproved {
		verify = func(txRepo *repository.GuardRepositories) error {
			stale, changes, err := s.detectDriftWithTx(ctx, txRepo, current.InputSnapshotJSON)
			if err != nil {
				return err
			}
			if stale {
				return web.Unprocessable("SCENARIO_INPUTS_CHANGED", buildStaleMessage(changes), nil)
			}
			return nil
		}
	}

	actor.Action = "layout_scenario.transition"
	if req.TargetStatus == constants.ScenarioApproved {
		actor.Action = "layout_scenario.approve"
	}
	actor.EntityType = "layout_scenario"
	actor.BeforeSummary = string(current.ScenarioStatus)
	actor.AfterSummary = fmt.Sprintf("%s reason=%s", req.TargetStatus, strings.TrimSpace(req.Reason))
	if err := s.scenarios.Transition(ctx, current, req.TargetStatus, actor.ActorID, actor, verify); err != nil {
		if webErr, ok := err.(*web.AppError); ok && webErr.Code == "SCENARIO_INPUTS_CHANGED" {
			s.recordBlockedApproval(ctx, id, current.ScenarioStatus, actor, webErr.Message)
		}
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

// attachFreshness fills the input drift fields of a scenario response. When
// the caller already loaded current zones/racks they are reused (the list
// endpoint loads them once for the whole page); otherwise they are fetched.
func (s *LayoutScenarioService) attachFreshness(ctx context.Context, item model.LayoutScenario, response *dto.ScenarioResponse, zones []model.ThermalZone, racks []model.Rack) error {
	input, err := planner.Decode(item.InputSnapshotJSON)
	if err != nil || !input.IsEvaluation() {
		// Drafts and legacy snapshots have no evaluated result to drift from.
		response.InputsFresh = true
		response.InputChanges = []dto.InputChange{}
		return nil
	}
	if zones == nil {
		zones, err = s.zones.All(ctx)
		if err != nil {
			return err
		}
	}
	if racks == nil {
		racks, err = s.racks.All(ctx)
		if err != nil {
			return err
		}
	}
	loads, err := s.loads.ExistingByIDs(ctx, input.LoadIDs)
	if err != nil {
		return err
	}
	changes := planner.Diff(input, zones, racks, loads, planner.AlgorithmVersion)
	response.InputChanges = toDTOChanges(changes)
	response.InputsFresh = len(changes) == 0
	response.InputsChangedSinceEval = len(changes) > 0
	return nil
}

// detectDriftWithTx performs the server-side approval re-check using reads in
// the transition transaction.
func (s *LayoutScenarioService) detectDriftWithTx(ctx context.Context, repos *repository.GuardRepositories, rawSnapshot string) (bool, []planner.Change, error) {
	input, err := planner.Decode(rawSnapshot)
	if err != nil {
		return false, nil, web.Unprocessable("INVALID_SCENARIO_SNAPSHOT", "scenario input snapshot cannot be verified", err)
	}
	if !input.IsEvaluation() {
		return false, nil, web.Unprocessable("SCENARIO_NOT_EVALUATED", "scenario must be evaluated before approval", nil)
	}
	zones, err := repos.Zones.All(ctx)
	if err != nil {
		return false, nil, err
	}
	racks, err := repos.Racks.All(ctx)
	if err != nil {
		return false, nil, err
	}
	loads, err := repos.Loads.ExistingByIDs(ctx, input.LoadIDs)
	if err != nil {
		return false, nil, err
	}
	changes := planner.Diff(input, zones, racks, loads, planner.AlgorithmVersion)
	return len(changes) > 0, changes, nil
}

func (s *LayoutScenarioService) recordBlockedApproval(ctx context.Context, id uint, from constants.ScenarioStatus, actor audit.Entry, reason string) {
	entry := audit.Entry{
		RequestID: actor.RequestID, ActorID: actor.ActorID, ActorUsername: actor.ActorUsername,
		Action: "layout_scenario.approve.blocked", EntityType: "layout_scenario", EntityID: id,
		BeforeSummary: string(from), AfterSummary: "blocked: " + reason,
	}
	_ = s.scenarios.RecordAudit(ctx, entry)
}

func toDTOChanges(changes []planner.Change) []dto.InputChange {
	result := make([]dto.InputChange, 0, len(changes))
	for _, change := range changes {
		fields := change.Fields
		if fields == nil {
			fields = []string{}
		}
		result = append(result, dto.InputChange{
			EntityType: change.EntityType, EntityID: change.EntityID, EntityCode: change.EntityCode,
			ChangeType: change.ChangeType, ChangedFields: fields, Description: change.Description,
		})
	}
	return result
}

func buildStaleMessage(changes []planner.Change) string {
	const maxShown = 5
	parts := make([]string, 0, maxShown)
	for i, change := range changes {
		if i >= maxShown {
			parts = append(parts, fmt.Sprintf("and %d more change(s)", len(changes)-maxShown))
			break
		}
		parts = append(parts, change.Description)
	}
	return "inputs changed since evaluation, re-evaluate before approval: " + strings.Join(parts, "; ")
}

func encodeJSON(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

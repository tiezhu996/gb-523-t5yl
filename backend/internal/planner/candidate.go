package planner

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"datacenter-thermal-capacity-planner/backend/internal/dto"
	"datacenter-thermal-capacity-planner/backend/internal/model"
)

const AlgorithmVersion = "thermal-v1"

type Engine struct {
	maxIterations int
}

type Result struct {
	Assignments []dto.RackAssignment
	ZoneResults []dto.ZoneThermalResult
	Violations  []dto.ConstraintViolation
	TotalPower  float64
	PeakTemp    float64
	Score       float64
}

type rackUsage struct {
	powerKW    float64
	heatKW     float64
	airflowCFM float64
	rackUnits  int
	groups     map[string]bool
}

type candidate struct {
	rack        model.Rack
	zone        model.ThermalZone
	score       float64
	explanation []string
}

func NewEngine(maxIterations int) *Engine {
	if maxIterations < 1 {
		maxIterations = 1
	}
	return &Engine{maxIterations: maxIterations}
}

func (e *Engine) Evaluate(zones []model.ThermalZone, racks []model.Rack, loads []model.EquipmentLoad) Result {
	zoneByID := make(map[uint]model.ThermalZone, len(zones))
	for _, zone := range zones {
		zoneByID[zone.ID] = zone
	}

	orderedRacks := append([]model.Rack(nil), racks...)
	sort.SliceStable(orderedRacks, func(i, j int) bool {
		if orderedRacks[i].RackCode == orderedRacks[j].RackCode {
			return orderedRacks[i].ID < orderedRacks[j].ID
		}
		return orderedRacks[i].RackCode < orderedRacks[j].RackCode
	})
	orderedLoads := append([]model.EquipmentLoad(nil), loads...)
	sort.SliceStable(orderedLoads, func(i, j int) bool {
		left := tightness(orderedLoads[i], orderedRacks)
		right := tightness(orderedLoads[j], orderedRacks)
		if left == right {
			if orderedLoads[i].PowerKW == orderedLoads[j].PowerKW {
				return orderedLoads[i].ID < orderedLoads[j].ID
			}
			return orderedLoads[i].PowerKW > orderedLoads[j].PowerKW
		}
		return left > right
	})

	usage := make(map[uint]*rackUsage, len(orderedRacks))
	zoneHeat := make(map[uint]float64, len(zones))
	zonePower := make(map[uint]float64, len(zones))
	zoneGroups := make(map[uint]map[string]bool, len(zones))
	for _, zone := range zones {
		zoneGroups[zone.ID] = map[string]bool{}
	}
	for _, rack := range orderedRacks {
		usage[rack.ID] = &rackUsage{groups: map[string]bool{}}
	}

	result := Result{Assignments: []dto.RackAssignment{}, ZoneResults: []dto.ZoneThermalResult{}, Violations: []dto.ConstraintViolation{}}
	iterations := 0
	for _, load := range orderedLoads {
		if !load.IsPlannable() {
			result.Violations = append(result.Violations, dto.ConstraintViolation{
				Code: "LOAD_NOT_READY", Severity: "critical", EntityType: "equipment_load", EntityID: load.ID,
				Message: "load is not in ready state and cannot be placed",
			})
			continue
		}
		candidates := make([]candidate, 0, len(orderedRacks))
		var evidence []dto.ConstraintViolation
		for _, rack := range orderedRacks {
			iterations++
			if iterations > e.maxIterations {
				evidence = append(evidence, dto.ConstraintViolation{
					Code: "ITERATION_LIMIT", Severity: "critical", EntityType: "equipment_load", EntityID: load.ID,
					Message: "candidate search reached the configured iteration limit",
				})
				break
			}
			zone, exists := zoneByID[rack.ZoneID]
			if !exists {
				continue
			}
			violations := checkCandidate(load, rack, zone, usage[rack.ID], zoneHeat[rack.ZoneID], zoneGroups[rack.ZoneID])
			if len(violations) > 0 {
				evidence = append(evidence, violations...)
				continue
			}
			score, explanation := placementScore(load, rack, zone, usage[rack.ID], zoneHeat[rack.ZoneID], zones, zoneHeat)
			candidates = append(candidates, candidate{rack: rack, zone: zone, score: score, explanation: explanation})
		}
		if len(candidates) == 0 {
			result.Violations = append(result.Violations, summarizeUnplaced(load, evidence)...)
			continue
		}
		sort.SliceStable(candidates, func(i, j int) bool {
			if candidates[i].score == candidates[j].score {
				return candidates[i].rack.RackCode < candidates[j].rack.RackCode
			}
			return candidates[i].score > candidates[j].score
		})
		selected := candidates[0]
		u := usage[selected.rack.ID]
		u.powerKW += load.PowerKW
		u.heatKW += load.HeatKW
		u.airflowCFM += load.AirflowCFM
		u.rackUnits += load.RackUnits
		u.groups[load.RedundancyGroup] = true
		zoneGroups[selected.zone.ID][load.RedundancyGroup] = true
		zoneHeat[selected.zone.ID] += load.HeatKW
		zonePower[selected.zone.ID] += load.PowerKW
		result.TotalPower += load.PowerKW
		result.Assignments = append(result.Assignments, dto.RackAssignment{
			LoadID: load.ID, LoadName: load.Name, RackID: selected.rack.ID, RackCode: selected.rack.RackCode,
			ZoneID: selected.zone.ID, ZoneCode: selected.zone.ZoneCode, PowerKW: load.PowerKW,
			HeatKW: load.HeatKW, AirflowCFM: load.AirflowCFM, RackUnits: load.RackUnits,
			PlacementScore: selected.score, Explanation: selected.explanation,
		})
	}

	thermalResults, thermalViolations, peak := propagateThermal(zones, zoneHeat)
	result.ZoneResults = thermalResults
	result.Violations = append(result.Violations, thermalViolations...)
	result.PeakTemp = peak
	result.Violations = append(result.Violations, validateFinalAssignments(orderedRacks, usage, zones, zonePower, result.Assignments)...)
	result.Score = scenarioScore(result.Assignments, result.ZoneResults, result.Violations)
	return result
}

func tightness(load model.EquipmentLoad, racks []model.Rack) float64 {
	best := 0.0
	for _, rack := range racks {
		if !rack.IsUsable() {
			continue
		}
		value := maxFloat(load.PowerKW/rack.PowerLimitKW, load.AirflowCFM/rack.AirflowLimitCFM, float64(load.RackUnits)/float64(rack.RackUnits))
		if value > best {
			best = value
		}
	}
	return best
}

// ---------------------------------------------------------------------------
// Evaluation input snapshots
//
// The engine consumes a deterministic set of zones, racks and loads. The
// snapshot types capture that input set at evaluation time and detect when
// planners later edit racks, zones or loads, so reviewers never approve a
// result produced from stale inputs.
// ---------------------------------------------------------------------------

// Kind separates a pre-evaluation draft snapshot from a full evaluation
// snapshot. Only evaluation snapshots can drift from the current inputs.
const (
	KindDraft      = "draft_inputs"
	KindEvaluation = "evaluation_inputs"
)

// Inputs is the complete deterministic input of one scenario evaluation.
type Inputs struct {
	Kind             string                `json:"kind"`
	LoadIDs          []uint                `json:"load_ids"`
	AlgorithmVersion string                `json:"algorithm_version"`
	Zones            []model.ThermalZone   `json:"zones"`
	Racks            []model.Rack          `json:"racks"`
	Loads            []model.EquipmentLoad `json:"loads"`
}

// Encode serializes the snapshot for storage in input_snapshot_json.
func Encode(input Inputs) (string, error) {
	if input.LoadIDs == nil {
		input.LoadIDs = []uint{}
	}
	if input.Zones == nil {
		input.Zones = []model.ThermalZone{}
	}
	if input.Racks == nil {
		input.Racks = []model.Rack{}
	}
	if input.Loads == nil {
		input.Loads = []model.EquipmentLoad{}
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// Decode parses a stored snapshot. Draft snapshots created before snapshot
// freshness was tracked only contain load ids and the algorithm version.
func Decode(raw string) (Inputs, error) {
	var input Inputs
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		return Inputs{}, err
	}
	return input, nil
}

// IsEvaluation reports whether the snapshot captured full evaluation inputs.
func (i Inputs) IsEvaluation() bool {
	return i.Kind == KindEvaluation || (i.Kind == "" && len(i.Zones) > 0)
}

const (
	EntityZone      = "thermal_zone"
	EntityRack      = "rack"
	EntityLoad      = "equipment_load"
	EntityAlgorithm = "algorithm"

	ChangeAdded    = "added"
	ChangeRemoved  = "removed"
	ChangeModified = "modified"

	floatEpsilon = 1e-9
)

// Change describes one way in which current planning inputs differ from the
// inputs captured by the evaluation snapshot.
type Change struct {
	EntityType  string   `json:"entity_type"`
	EntityID    uint     `json:"entity_id"`
	EntityCode  string   `json:"entity_code"`
	ChangeType  string   `json:"change_type"`
	Fields      []string `json:"changed_fields,omitempty"`
	Description string   `json:"description"`
}

// Diff compares an evaluation snapshot against the current zones, racks and
// the scenario's selected loads. It returns a deterministic, empty-when-fresh
// list of changes. Every zone and rack is an evaluation input because the
// placement engine scans all racks and propagates heat across all zones;
// only the loads selected by the snapshot participate in the evaluation.
func Diff(snap Inputs, zones []model.ThermalZone, racks []model.Rack, loads []model.EquipmentLoad, algorithmVersion string) []Change {
	changes := []Change{}
	if snap.AlgorithmVersion != "" && snap.AlgorithmVersion != algorithmVersion {
		changes = append(changes, Change{
			EntityType:  EntityAlgorithm,
			EntityCode:  algorithmVersion,
			ChangeType:  ChangeModified,
			Fields:      []string{"algorithm_version"},
			Description: fmt.Sprintf("planning algorithm changed from %s to %s", snap.AlgorithmVersion, algorithmVersion),
		})
	}
	changes = append(changes, diffZones(snap.Zones, zones)...)
	changes = append(changes, diffRacks(snap.Racks, racks)...)
	changes = append(changes, diffLoads(snap.Loads, loads)...)
	sortChanges(changes)
	return changes
}

func diffZones(snapshot, current []model.ThermalZone) []Change {
	changes := []Change{}
	currentByID := make(map[uint]model.ThermalZone, len(current))
	for _, zone := range current {
		currentByID[zone.ID] = zone
	}
	snapByID := make(map[uint]model.ThermalZone, len(snapshot))
	for _, zone := range snapshot {
		snapByID[zone.ID] = zone
		if next, ok := currentByID[zone.ID]; !ok {
			changes = append(changes, Change{
				EntityType: EntityZone, EntityID: zone.ID, EntityCode: zone.ZoneCode, ChangeType: ChangeRemoved,
				Description: fmt.Sprintf("thermal zone %s was removed after evaluation", zone.ZoneCode),
			})
		} else if fields := changedZoneFields(zone, next); len(fields) > 0 {
			changes = append(changes, Change{
				EntityType: EntityZone, EntityID: zone.ID, EntityCode: zone.ZoneCode, ChangeType: ChangeModified, Fields: fields,
				Description: fmt.Sprintf("thermal zone %s changed: %s", zone.ZoneCode, joinFields(fields)),
			})
		}
	}
	for _, zone := range current {
		if _, ok := snapByID[zone.ID]; !ok {
			changes = append(changes, Change{
				EntityType: EntityZone, EntityID: zone.ID, EntityCode: zone.ZoneCode, ChangeType: ChangeAdded,
				Description: fmt.Sprintf("thermal zone %s was added after evaluation", zone.ZoneCode),
			})
		}
	}
	return changes
}

func changedZoneFields(before, after model.ThermalZone) []string {
	fields := []string{}
	if before.Name != after.Name {
		fields = append(fields, "name")
	}
	if !floatEqual(before.CoolingCapacityKW, after.CoolingCapacityKW) {
		fields = append(fields, "cooling_capacity_kw")
	}
	if !floatEqual(before.SupplyTempC, after.SupplyTempC) {
		fields = append(fields, "supply_temp_c")
	}
	if !floatEqual(before.MaxReturnTempC, after.MaxReturnTempC) {
		fields = append(fields, "max_return_temp_c")
	}
	if before.ZoneStatus != after.ZoneStatus {
		fields = append(fields, "zone_status")
	}
	if !jsonEqual(before.AdjacencyJSON, after.AdjacencyJSON) {
		fields = append(fields, "adjacency")
	}
	return fields
}

func diffRacks(snapshot, current []model.Rack) []Change {
	changes := []Change{}
	currentByID := make(map[uint]model.Rack, len(current))
	for _, rack := range current {
		currentByID[rack.ID] = rack
	}
	snapByID := make(map[uint]model.Rack, len(snapshot))
	for _, rack := range snapshot {
		snapByID[rack.ID] = rack
		if next, ok := currentByID[rack.ID]; !ok {
			changes = append(changes, Change{
				EntityType: EntityRack, EntityID: rack.ID, EntityCode: rack.RackCode, ChangeType: ChangeRemoved,
				Description: fmt.Sprintf("rack %s was removed after evaluation", rack.RackCode),
			})
		} else if fields := changedRackFields(rack, next); len(fields) > 0 {
			changes = append(changes, Change{
				EntityType: EntityRack, EntityID: rack.ID, EntityCode: rack.RackCode, ChangeType: ChangeModified, Fields: fields,
				Description: fmt.Sprintf("rack %s changed: %s", rack.RackCode, joinFields(fields)),
			})
		}
	}
	for _, rack := range current {
		if _, ok := snapByID[rack.ID]; !ok {
			changes = append(changes, Change{
				EntityType: EntityRack, EntityID: rack.ID, EntityCode: rack.RackCode, ChangeType: ChangeAdded,
				Description: fmt.Sprintf("rack %s was added after evaluation", rack.RackCode),
			})
		}
	}
	return changes
}

func changedRackFields(before, after model.Rack) []string {
	fields := []string{}
	if before.ZoneID != after.ZoneID {
		fields = append(fields, "zone_id")
	}
	if before.RackCode != after.RackCode {
		fields = append(fields, "rack_code")
	}
	if before.RowIndex != after.RowIndex {
		fields = append(fields, "row_index")
	}
	if before.ColumnIndex != after.ColumnIndex {
		fields = append(fields, "column_index")
	}
	if !floatEqual(before.PowerLimitKW, after.PowerLimitKW) {
		fields = append(fields, "power_limit_kw")
	}
	if !floatEqual(before.AirflowLimitCFM, after.AirflowLimitCFM) {
		fields = append(fields, "airflow_limit_cfm")
	}
	if before.RackUnits != after.RackUnits {
		fields = append(fields, "rack_units")
	}
	if before.RackStatus != after.RackStatus {
		fields = append(fields, "rack_status")
	}
	return fields
}

func diffLoads(snapshot, current []model.EquipmentLoad) []Change {
	changes := []Change{}
	currentByID := make(map[uint]model.EquipmentLoad, len(current))
	for _, load := range current {
		currentByID[load.ID] = load
	}
	for _, load := range snapshot {
		if next, ok := currentByID[load.ID]; !ok {
			changes = append(changes, Change{
				EntityType: EntityLoad, EntityID: load.ID, EntityCode: load.Name, ChangeType: ChangeRemoved,
				Description: fmt.Sprintf("equipment load %s was removed after evaluation", load.Name),
			})
		} else if fields := changedLoadFields(load, next); len(fields) > 0 {
			changes = append(changes, Change{
				EntityType: EntityLoad, EntityID: load.ID, EntityCode: load.Name, ChangeType: ChangeModified, Fields: fields,
				Description: fmt.Sprintf("equipment load %s changed: %s", load.Name, joinFields(fields)),
			})
		}
	}
	// Loads created after the evaluation are not part of its inputs, so only
	// snapshot loads are compared; there are no "added" load changes.
	return changes
}

func changedLoadFields(before, after model.EquipmentLoad) []string {
	fields := []string{}
	if before.Name != after.Name {
		fields = append(fields, "name")
	}
	if !floatEqual(before.PowerKW, after.PowerKW) {
		fields = append(fields, "power_kw")
	}
	if !floatEqual(before.HeatKW, after.HeatKW) {
		fields = append(fields, "heat_kw")
	}
	if !floatEqual(before.AirflowCFM, after.AirflowCFM) {
		fields = append(fields, "airflow_cfm")
	}
	if before.RackUnits != after.RackUnits {
		fields = append(fields, "rack_units")
	}
	if before.RedundancyGroup != after.RedundancyGroup {
		fields = append(fields, "redundancy_group")
	}
	if !uintPtrEqual(before.PreferredZoneID, after.PreferredZoneID) {
		fields = append(fields, "preferred_zone_id")
	}
	if before.LoadStatus != after.LoadStatus {
		fields = append(fields, "load_status")
	}
	return fields
}

func floatEqual(left, right float64) bool {
	return math.Abs(left-right) <= floatEpsilon
}

func uintPtrEqual(left, right *uint) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func jsonEqual(left, right string) bool {
	var leftValue, rightValue any
	if err := json.Unmarshal([]byte(left), &leftValue); err != nil {
		return left == right
	}
	if err := json.Unmarshal([]byte(right), &rightValue); err != nil {
		return left == right
	}
	leftCanonical, _ := json.Marshal(leftValue)
	rightCanonical, _ := json.Marshal(rightValue)
	return string(leftCanonical) == string(rightCanonical)
}

func joinFields(fields []string) string {
	if len(fields) == 0 {
		return ""
	}
	out := fields[0]
	for _, field := range fields[1:] {
		out += ", " + field
	}
	return out
}

func sortChanges(changes []Change) {
	sort.SliceStable(changes, func(i, j int) bool {
		if changes[i].EntityType != changes[j].EntityType {
			return changes[i].EntityType < changes[j].EntityType
		}
		if changes[i].EntityID != changes[j].EntityID {
			return changes[i].EntityID < changes[j].EntityID
		}
		return changes[i].ChangeType < changes[j].ChangeType
	})
}

package planner

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"datacenter-thermal-capacity-planner/backend/internal/dto"
	"datacenter-thermal-capacity-planner/backend/internal/model"
)

// propagateThermal is an intentionally simplified planning model. It estimates
// return temperature from direct and weighted neighboring heat, not sensor data.
func propagateThermal(zones []model.ThermalZone, directHeat map[uint]float64) ([]dto.ZoneThermalResult, []dto.ConstraintViolation, float64) {
	ordered := append([]model.ThermalZone(nil), zones...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ZoneCode < ordered[j].ZoneCode })
	byCode := make(map[string]model.ThermalZone, len(zones))
	for _, zone := range zones {
		byCode[zone.ZoneCode] = zone
	}
	results := make([]dto.ZoneThermalResult, 0, len(zones))
	violations := []dto.ConstraintViolation{}
	peak := 0.0
	for _, zone := range ordered {
		neighborHeat := 0.0
		for code, weight := range dto.DecodeAdjacency(zone.AdjacencyJSON) {
			if neighbor, ok := byCode[code]; ok {
				neighborHeat += directHeat[neighbor.ID] * weight
			}
		}
		effectiveHeat := directHeat[zone.ID] + neighborHeat
		capacity := zone.CoolingCapacityKW
		utilization := 0.0
		if capacity > 0 {
			utilization = effectiveHeat / capacity
		}
		// A calibrated planning assumption: full envelope adds 12 C to supply.
		estimated := zone.SupplyTempC + utilization*12
		margin := zone.MaxReturnTempC - estimated
		coolingMargin := capacity - effectiveHeat
		results = append(results, dto.ZoneThermalResult{
			ZoneID: zone.ID, ZoneCode: zone.ZoneCode, AssignedHeatKW: round2(directHeat[zone.ID]),
			NeighborHeatKW: round2(neighborHeat), EstimatedReturnC: round2(estimated),
			TemperatureMarginC: round2(margin), CoolingMarginKW: round2(coolingMargin),
		})
		if estimated > peak {
			peak = estimated
		}
		if coolingMargin < 0 {
			violations = append(violations, violation("ZONE_COOLING_LIMIT", zone.ID, "thermal_zone", "effective heat including adjacency exceeds cooling capacity", effectiveHeat, capacity))
		}
		if margin < 0 {
			violations = append(violations, violation("ZONE_RETURN_TEMP", zone.ID, "thermal_zone", "estimated return temperature exceeds configured limit", estimated, zone.MaxReturnTempC))
		} else if margin < 2 {
			violations = append(violations, dto.ConstraintViolation{
				Code: "ZONE_TEMP_HEADROOM_LOW", Severity: "warning", EntityType: "thermal_zone", EntityID: zone.ID,
				Message: "estimated return temperature has less than 2 C headroom", Actual: round2(estimated), Limit: zone.MaxReturnTempC,
			})
		}
	}
	return results, violations, round2(peak)
}

// EvaluationSnapshot is the complete planning input captured when a scenario
// is evaluated. Draft scenarios only carry load ids, so Zones/Racks/Loads are
// empty until the first evaluation.
type EvaluationSnapshot struct {
	LoadIDs          []uint                `json:"load_ids"`
	AlgorithmVersion string                `json:"algorithm_version"`
	Zones            []model.ThermalZone   `json:"zones"`
	Racks            []model.Rack          `json:"racks"`
	Loads            []model.EquipmentLoad `json:"loads"`
}

const changeEpsilon = 1e-6

// DecodeSnapshot parses the stored input snapshot. It returns ok=false when
// the snapshot cannot be decoded or has never contained a full evaluation.
func DecodeSnapshot(raw string) (EvaluationSnapshot, bool) {
	var snapshot EvaluationSnapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return EvaluationSnapshot{}, false
	}
	return snapshot, len(snapshot.Zones) > 0 || len(snapshot.Racks) > 0 || len(snapshot.Loads) > 0
}

// DiffInputs compares the snapshot taken at evaluation time with the current
// planning inputs and returns every changed rack, thermal zone or selected
// load. New racks and zones are reported because they can change placement;
// new loads are ignored unless they were part of the scenario selection.
func DiffInputs(snapshot EvaluationSnapshot, zones []model.ThermalZone, racks []model.Rack, loads []model.EquipmentLoad) []dto.InputChange {
	changes := []dto.InputChange{}
	changes = append(changes, diffZones(snapshot.Zones, zones)...)
	changes = append(changes, diffRacks(snapshot.Racks, racks)...)
	changes = append(changes, diffLoads(snapshot.Loads, loads)...)
	sortChanges(changes)
	return changes
}

func diffZones(snapshot, current []model.ThermalZone) []dto.InputChange {
	changes := []dto.InputChange{}
	byID := make(map[uint]model.ThermalZone, len(current))
	for _, zone := range current {
		byID[zone.ID] = zone
	}
	seen := map[uint]bool{}
	for _, old := range snapshot {
		seen[old.ID] = true
		next, exists := byID[old.ID]
		if !exists {
			changes = append(changes, dto.InputChange{
				EntityType: "thermal_zone", EntityID: old.ID, Code: old.ZoneCode,
				ChangeType: "removed", Field: "", Detail: fmt.Sprintf("thermal zone %s was removed", old.ZoneCode),
			})
			continue
		}
		label := old.ZoneCode
		if !stringsEqual(old.Name, next.Name) {
			changes = append(changes, textChange("thermal_zone", next.ID, label, "name", old.Name, next.Name))
		}
		if !floatEqual(old.CoolingCapacityKW, next.CoolingCapacityKW) {
			changes = append(changes, numericChange("thermal_zone", next.ID, label, "cooling_capacity_kw", old.CoolingCapacityKW, next.CoolingCapacityKW, "kW"))
		}
		if !floatEqual(old.SupplyTempC, next.SupplyTempC) {
			changes = append(changes, numericChange("thermal_zone", next.ID, label, "supply_temp_c", old.SupplyTempC, next.SupplyTempC, "C"))
		}
		if !floatEqual(old.MaxReturnTempC, next.MaxReturnTempC) {
			changes = append(changes, numericChange("thermal_zone", next.ID, label, "max_return_temp_c", old.MaxReturnTempC, next.MaxReturnTempC, "C"))
		}
		if !adjacencyEqual(old.AdjacencyJSON, next.AdjacencyJSON) {
			changes = append(changes, dto.InputChange{
				EntityType: "thermal_zone", EntityID: next.ID, Code: label,
				ChangeType: "updated", Field: "adjacency_json",
				OldValue: strings.TrimSpace(old.AdjacencyJSON), NewValue: strings.TrimSpace(next.AdjacencyJSON),
				Detail: fmt.Sprintf("thermal zone %s adjacency weights changed", label),
			})
		}
		if !stringsEqual(old.ZoneStatus, next.ZoneStatus) {
			changes = append(changes, textChange("thermal_zone", next.ID, label, "zone_status", old.ZoneStatus, next.ZoneStatus))
		}
	}
	for _, zone := range current {
		if !seen[zone.ID] {
			changes = append(changes, dto.InputChange{
				EntityType: "thermal_zone", EntityID: zone.ID, Code: zone.ZoneCode,
				ChangeType: "added", Field: "", Detail: fmt.Sprintf("thermal zone %s was added", zone.ZoneCode),
			})
		}
	}
	return changes
}

func diffRacks(snapshot, current []model.Rack) []dto.InputChange {
	changes := []dto.InputChange{}
	byID := make(map[uint]model.Rack, len(current))
	for _, rack := range current {
		byID[rack.ID] = rack
	}
	seen := map[uint]bool{}
	for _, old := range snapshot {
		seen[old.ID] = true
		next, exists := byID[old.ID]
		if !exists {
			changes = append(changes, dto.InputChange{
				EntityType: "rack", EntityID: old.ID, Code: old.RackCode,
				ChangeType: "removed", Field: "", Detail: fmt.Sprintf("rack %s was removed", old.RackCode),
			})
			continue
		}
		label := old.RackCode
		if old.ZoneID != next.ZoneID {
			changes = append(changes, dto.InputChange{
				EntityType: "rack", EntityID: next.ID, Code: label,
				ChangeType: "updated", Field: "zone_id",
				OldValue: fmt.Sprintf("%d", old.ZoneID), NewValue: fmt.Sprintf("%d", next.ZoneID),
				Detail: fmt.Sprintf("rack %s moved to another thermal zone", label),
			})
		}
		if old.RowIndex != next.RowIndex {
			changes = append(changes, intChange("rack", next.ID, label, "row_index", old.RowIndex, next.RowIndex))
		}
		if old.ColumnIndex != next.ColumnIndex {
			changes = append(changes, intChange("rack", next.ID, label, "column_index", old.ColumnIndex, next.ColumnIndex))
		}
		if !floatEqual(old.PowerLimitKW, next.PowerLimitKW) {
			changes = append(changes, numericChange("rack", next.ID, label, "power_limit_kw", old.PowerLimitKW, next.PowerLimitKW, "kW"))
		}
		if !floatEqual(old.AirflowLimitCFM, next.AirflowLimitCFM) {
			changes = append(changes, numericChange("rack", next.ID, label, "airflow_limit_cfm", old.AirflowLimitCFM, next.AirflowLimitCFM, "CFM"))
		}
		if old.RackUnits != next.RackUnits {
			changes = append(changes, intChange("rack", next.ID, label, "rack_units", old.RackUnits, next.RackUnits))
		}
		if !stringsEqual(string(old.RackStatus), string(next.RackStatus)) {
			changes = append(changes, textChange("rack", next.ID, label, "rack_status", string(old.RackStatus), string(next.RackStatus)))
		}
	}
	for _, rack := range current {
		if !seen[rack.ID] {
			changes = append(changes, dto.InputChange{
				EntityType: "rack", EntityID: rack.ID, Code: rack.RackCode,
				ChangeType: "added", Field: "", Detail: fmt.Sprintf("rack %s was added", rack.RackCode),
			})
		}
	}
	return changes
}

func diffLoads(snapshot, current []model.EquipmentLoad) []dto.InputChange {
	changes := []dto.InputChange{}
	byID := make(map[uint]model.EquipmentLoad, len(current))
	for _, load := range current {
		byID[load.ID] = load
	}
	for _, old := range snapshot {
		label := old.Name
		next, exists := byID[old.ID]
		if !exists {
			changes = append(changes, dto.InputChange{
				EntityType: "equipment_load", EntityID: old.ID, Code: label,
				ChangeType: "removed", Field: "", Detail: fmt.Sprintf("equipment load %q was removed", label),
			})
			continue
		}
		if !stringsEqual(old.Name, next.Name) {
			changes = append(changes, textChange("equipment_load", next.ID, label, "name", old.Name, next.Name))
		}
		if !floatEqual(old.PowerKW, next.PowerKW) {
			changes = append(changes, numericChange("equipment_load", next.ID, label, "power_kw", old.PowerKW, next.PowerKW, "kW"))
		}
		if !floatEqual(old.HeatKW, next.HeatKW) {
			changes = append(changes, numericChange("equipment_load", next.ID, label, "heat_kw", old.HeatKW, next.HeatKW, "kW"))
		}
		if !floatEqual(old.AirflowCFM, next.AirflowCFM) {
			changes = append(changes, numericChange("equipment_load", next.ID, label, "airflow_cfm", old.AirflowCFM, next.AirflowCFM, "CFM"))
		}
		if old.RackUnits != next.RackUnits {
			changes = append(changes, intChange("equipment_load", next.ID, label, "rack_units", old.RackUnits, next.RackUnits))
		}
		if !stringsEqual(old.RedundancyGroup, next.RedundancyGroup) {
			changes = append(changes, textChange("equipment_load", next.ID, label, "redundancy_group", old.RedundancyGroup, next.RedundancyGroup))
		}
		if !nullableIDEqual(old.PreferredZoneID, next.PreferredZoneID) {
			changes = append(changes, dto.InputChange{
				EntityType: "equipment_load", EntityID: next.ID, Code: label,
				ChangeType: "updated", Field: "preferred_zone_id",
				OldValue: nullableIDText(old.PreferredZoneID), NewValue: nullableIDText(next.PreferredZoneID),
				Detail: fmt.Sprintf("equipment load %q preferred thermal zone changed", label),
			})
		}
		if !stringsEqual(old.LoadStatus, next.LoadStatus) {
			changes = append(changes, textChange("equipment_load", next.ID, label, "load_status", old.LoadStatus, next.LoadStatus))
		}
	}
	return changes
}

func textChange(entityType string, id uint, code, field, oldValue, newValue string) dto.InputChange {
	return dto.InputChange{
		EntityType: entityType, EntityID: id, Code: code, ChangeType: "updated", Field: field,
		OldValue: oldValue, NewValue: newValue,
		Detail: fmt.Sprintf("%s %s %s changed from %q to %q", entityLabel(entityType), code, field, oldValue, newValue),
	}
}

func numericChange(entityType string, id uint, code, field string, oldValue, newValue float64, unit string) dto.InputChange {
	return dto.InputChange{
		EntityType: entityType, EntityID: id, Code: code, ChangeType: "updated", Field: field,
		OldValue: fmt.Sprintf("%s %s", formatValue(oldValue), unit),
		NewValue: fmt.Sprintf("%s %s", formatValue(newValue), unit),
		Detail:   fmt.Sprintf("%s %s %s changed from %s to %s %s", entityLabel(entityType), code, field, formatValue(oldValue), formatValue(newValue), unit),
	}
}

func intChange(entityType string, id uint, code, field string, oldValue, newValue int) dto.InputChange {
	return dto.InputChange{
		EntityType: entityType, EntityID: id, Code: code, ChangeType: "updated", Field: field,
		OldValue: fmt.Sprintf("%d", oldValue), NewValue: fmt.Sprintf("%d", newValue),
		Detail: fmt.Sprintf("%s %s %s changed from %d to %d", entityLabel(entityType), code, field, oldValue, newValue),
	}
}

func entityLabel(entityType string) string {
	switch entityType {
	case "thermal_zone":
		return "thermal zone"
	case "rack":
		return "rack"
	case "equipment_load":
		return "equipment load"
	default:
		return entityType
	}
}

func formatValue(value float64) string {
	rounded := math.Round(value*100) / 100
	return fmt.Sprintf("%.2f", rounded)
}

func floatEqual(left, right float64) bool {
	return math.Abs(left-right) < changeEpsilon
}

func stringsEqual(left, right string) bool {
	return left == right
}

func nullableIDEqual(left, right *uint) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func nullableIDText(value *uint) string {
	if value == nil {
		return "none"
	}
	return fmt.Sprintf("%d", *value)
}

// adjacencyEqual compares decoded adjacency weights so JSON key ordering never
// produces a false stale result.
func adjacencyEqual(left, right string) bool {
	var leftMap, rightMap map[string]float64
	if err := json.Unmarshal([]byte(left), &leftMap); err != nil {
		return strings.TrimSpace(left) == strings.TrimSpace(right)
	}
	if err := json.Unmarshal([]byte(right), &rightMap); err != nil {
		return false
	}
	if len(leftMap) != len(rightMap) {
		return false
	}
	for code, weight := range leftMap {
		other, exists := rightMap[code]
		if !exists || !floatEqual(weight, other) {
			return false
		}
	}
	return true
}

func sortChanges(changes []dto.InputChange) {
	rank := map[string]int{"thermal_zone": 0, "rack": 1, "equipment_load": 2}
	sort.SliceStable(changes, func(i, j int) bool {
		if changes[i].EntityType != changes[j].EntityType {
			return rank[changes[i].EntityType] < rank[changes[j].EntityType]
		}
		if changes[i].EntityID != changes[j].EntityID {
			return changes[i].EntityID < changes[j].EntityID
		}
		return changes[i].Field < changes[j].Field
	})
}

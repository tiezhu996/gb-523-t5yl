package planner

import (
	"encoding/json"
	"testing"

	"datacenter-thermal-capacity-planner/backend/internal/constants"
	"datacenter-thermal-capacity-planner/backend/internal/model"
)

func freshnessFixture() (EvaluationSnapshot, []model.ThermalZone, []model.Rack, []model.EquipmentLoad) {
	zoneID := uint(1)
	zones := []model.ThermalZone{
		{ID: 1, ZoneCode: "TZ-A", Name: "Aisle A", CoolingCapacityKW: 100, SupplyTempC: 18, MaxReturnTempC: 30, AdjacencyJSON: `{"TZ-B":0.2}`, ZoneStatus: "active"},
		{ID: 2, ZoneCode: "TZ-B", Name: "Aisle B", CoolingCapacityKW: 80, SupplyTempC: 19, MaxReturnTempC: 31, AdjacencyJSON: `{}`, ZoneStatus: "active"},
	}
	racks := []model.Rack{
		{ID: 1, ZoneID: 1, RackCode: "A-01", RowIndex: 1, ColumnIndex: 1, PowerLimitKW: 20, AirflowLimitCFM: 6000, RackUnits: 42, RackStatus: constants.RackAvailable},
		{ID: 2, ZoneID: 2, RackCode: "B-01", RowIndex: 2, ColumnIndex: 1, PowerLimitKW: 16, AirflowLimitCFM: 5000, RackUnits: 42, RackStatus: constants.RackReserved},
	}
	loads := []model.EquipmentLoad{
		{ID: 1, Name: "node-1", PowerKW: 10, HeatKW: 9.5, AirflowCFM: 3000, RackUnits: 8, RedundancyGroup: "G1", PreferredZoneID: &zoneID, LoadStatus: "ready"},
	}
	snapshot := EvaluationSnapshot{
		LoadIDs:          []uint{1},
		AlgorithmVersion: AlgorithmVersion,
		Zones:            append([]model.ThermalZone(nil), zones...),
		Racks:            append([]model.Rack(nil), racks...),
		Loads:            append([]model.EquipmentLoad(nil), loads...),
	}
	return snapshot, zones, racks, loads
}

func TestDiffInputs(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(zones *[]model.ThermalZone, racks *[]model.Rack, loads *[]model.EquipmentLoad)
		wantCount  int
		wantType   string
		wantField  string
		wantChange string
	}{
		{
			name:      "identical inputs",
			mutate:    func(*[]model.ThermalZone, *[]model.Rack, *[]model.EquipmentLoad) {},
			wantCount: 0,
		},
		{
			name: "zone cooling changed",
			mutate: func(z *[]model.ThermalZone, _ *[]model.Rack, _ *[]model.EquipmentLoad) {
				(*z)[0].CoolingCapacityKW = 120
			},
			wantCount:  1,
			wantType:   "thermal_zone",
			wantField:  "cooling_capacity_kw",
			wantChange: "updated",
		},
		{
			name: "zone added",
			mutate: func(z *[]model.ThermalZone, _ *[]model.Rack, _ *[]model.EquipmentLoad) {
				*z = append(*z, model.ThermalZone{ID: 3, ZoneCode: "TZ-C", AdjacencyJSON: `{}`, ZoneStatus: "active"})
			},
			wantCount:  1,
			wantType:   "thermal_zone",
			wantChange: "added",
		},
		{
			name:       "zone removed",
			mutate:     func(z *[]model.ThermalZone, _ *[]model.Rack, _ *[]model.EquipmentLoad) { *z = (*z)[:1] },
			wantCount:  1,
			wantType:   "thermal_zone",
			wantChange: "removed",
		},
		{
			name: "adjacency key order ignored",
			mutate: func(z *[]model.ThermalZone, _ *[]model.Rack, _ *[]model.EquipmentLoad) {
				(*z)[0].AdjacencyJSON = `{"TZ-B": 0.20}`
			},
			wantCount: 0,
		},
		{
			name: "adjacency weight changed",
			mutate: func(z *[]model.ThermalZone, _ *[]model.Rack, _ *[]model.EquipmentLoad) {
				(*z)[0].AdjacencyJSON = `{"TZ-B":0.9}`
			},
			wantCount:  1,
			wantType:   "thermal_zone",
			wantField:  "adjacency_json",
			wantChange: "updated",
		},
		{
			name:       "rack power limit changed",
			mutate:     func(_ *[]model.ThermalZone, r *[]model.Rack, _ *[]model.EquipmentLoad) { (*r)[0].PowerLimitKW = 30 },
			wantCount:  1,
			wantType:   "rack",
			wantField:  "power_limit_kw",
			wantChange: "updated",
		},
		{
			name: "rack status changed",
			mutate: func(_ *[]model.ThermalZone, r *[]model.Rack, _ *[]model.EquipmentLoad) {
				(*r)[1].RackStatus = constants.RackAvailable
			},
			wantCount:  1,
			wantType:   "rack",
			wantField:  "rack_status",
			wantChange: "updated",
		},
		{
			name: "rack added",
			mutate: func(_ *[]model.ThermalZone, r *[]model.Rack, _ *[]model.EquipmentLoad) {
				*r = append(*r, model.Rack{ID: 3, RackCode: "C-01", RackStatus: constants.RackAvailable})
			},
			wantCount:  1,
			wantType:   "rack",
			wantChange: "added",
		},
		{
			name:       "rack removed",
			mutate:     func(_ *[]model.ThermalZone, r *[]model.Rack, _ *[]model.EquipmentLoad) { *r = (*r)[:1] },
			wantCount:  1,
			wantType:   "rack",
			wantChange: "removed",
		},
		{
			name:       "load heat changed",
			mutate:     func(_ *[]model.ThermalZone, _ *[]model.Rack, l *[]model.EquipmentLoad) { (*l)[0].HeatKW = 4 },
			wantCount:  1,
			wantType:   "equipment_load",
			wantField:  "heat_kw",
			wantChange: "updated",
		},
		{
			name:       "load status changed",
			mutate:     func(_ *[]model.ThermalZone, _ *[]model.Rack, l *[]model.EquipmentLoad) { (*l)[0].LoadStatus = "draft" },
			wantCount:  1,
			wantType:   "equipment_load",
			wantField:  "load_status",
			wantChange: "updated",
		},
		{
			name:       "load removed",
			mutate:     func(_ *[]model.ThermalZone, _ *[]model.Rack, l *[]model.EquipmentLoad) { *l = []model.EquipmentLoad{} },
			wantCount:  1,
			wantType:   "equipment_load",
			wantChange: "removed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot, zones, racks, loads := freshnessFixture()
			tt.mutate(&zones, &racks, &loads)
			changes := DiffInputs(snapshot, zones, racks, loads)
			if len(changes) != tt.wantCount {
				t.Fatalf("change count=%d want %d: %+v", len(changes), tt.wantCount, changes)
			}
			if tt.wantCount > 0 {
				found := false
				for _, change := range changes {
					if change.EntityType == tt.wantType && change.ChangeType == tt.wantChange && change.Field == tt.wantField {
						found = true
					}
				}
				if !found {
					t.Fatalf("expected type=%s field=%s change=%s in %+v", tt.wantType, tt.wantField, tt.wantChange, changes)
				}
			}
		})
	}
}

func TestDiffInputsOrdersChanges(t *testing.T) {
	snapshot, zones, racks, loads := freshnessFixture()
	loads[0].PowerKW = 20
	racks[0].RackUnits = 48
	zones[1].ZoneStatus = "constrained"
	changes := DiffInputs(snapshot, zones, racks, loads)
	if len(changes) != 3 {
		t.Fatalf("expected 3 changes, got %+v", changes)
	}
	if changes[0].EntityType != "thermal_zone" || changes[1].EntityType != "rack" || changes[2].EntityType != "equipment_load" {
		t.Fatalf("changes must be grouped zone/rack/load: %+v", changes)
	}
}

func TestDecodeSnapshot(t *testing.T) {
	snapshot, _, _, _ := freshnessFixture()
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("encode snapshot: %v", err)
	}
	decoded, evaluated := DecodeSnapshot(string(raw))
	if !evaluated {
		t.Fatal("full evaluation snapshot must be detected as evaluated")
	}
	if len(decoded.Zones) != 2 || len(decoded.Racks) != 2 || len(decoded.Loads) != 1 {
		t.Fatalf("decoded snapshot mismatch: %+v", decoded)
	}
	if _, draft := DecodeSnapshot(`{"load_ids":[1],"algorithm_version":"thermal-v1"}`); draft {
		t.Fatal("draft snapshot without inputs must not be treated as evaluated")
	}
	if _, bad := DecodeSnapshot(`{not-json`); bad {
		t.Fatal("malformed snapshot must not be treated as evaluated")
	}
}

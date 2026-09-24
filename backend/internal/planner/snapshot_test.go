package planner

import (
	"testing"

	"datacenter-thermal-capacity-planner/backend/internal/constants"
	"datacenter-thermal-capacity-planner/backend/internal/model"
)

func zonePtr(id uint) *uint { return &id }

func baseInputs() Inputs {
	return Inputs{
		Kind:             KindEvaluation,
		LoadIDs:          []uint{1, 2},
		AlgorithmVersion: "thermal-v1",
		Zones: []model.ThermalZone{
			{ID: 1, ZoneCode: "Z1", Name: "Zone 1", CoolingCapacityKW: 100, SupplyTempC: 18, MaxReturnTempC: 32, AdjacencyJSON: `{"Z2":0.5}`, ZoneStatus: "active"},
		},
		Racks: []model.Rack{
			{ID: 1, ZoneID: 1, RackCode: "R1", RowIndex: 0, ColumnIndex: 0, PowerLimitKW: 10, AirflowLimitCFM: 1000, RackUnits: 42, RackStatus: constants.RackAvailable},
		},
		Loads: []model.EquipmentLoad{
			{ID: 1, Name: "load-a", PowerKW: 2, HeatKW: 1.8, AirflowCFM: 200, RackUnits: 10, RedundancyGroup: "g1", PreferredZoneID: zonePtr(1), LoadStatus: "ready"},
			{ID: 2, Name: "load-b", PowerKW: 3, HeatKW: 2.7, AirflowCFM: 300, RackUnits: 8, RedundancyGroup: "g2", LoadStatus: "ready"},
		},
	}
}

func TestDiff(t *testing.T) {
	cases := []struct {
		name          string
		mutate        func(*Inputs, *[]model.ThermalZone, *[]model.Rack, *[]model.EquipmentLoad)
		wantChanges   int
		wantEntity    string
		wantType      string
		wantFields    []string
		wantAlgorithm string
	}{
		{
			name:        "fresh identical inputs",
			mutate:      func(*Inputs, *[]model.ThermalZone, *[]model.Rack, *[]model.EquipmentLoad) {},
			wantChanges: 0,
		},
		{
			name: "zone cooling capacity modified",
			mutate: func(_ *Inputs, zones *[]model.ThermalZone, _ *[]model.Rack, _ *[]model.EquipmentLoad) {
				(*zones)[0].CoolingCapacityKW = 80
			},
			wantChanges: 1, wantEntity: EntityZone, wantType: ChangeModified, wantFields: []string{"cooling_capacity_kw"},
		},
		{
			name: "zone adjacency key order ignored",
			mutate: func(_ *Inputs, zones *[]model.ThermalZone, _ *[]model.Rack, _ *[]model.EquipmentLoad) {
				(*zones)[0].AdjacencyJSON = `{ "Z2" : 0.50 }`
			},
			wantChanges: 0,
		},
		{
			name: "zone added",
			mutate: func(_ *Inputs, zones *[]model.ThermalZone, _ *[]model.Rack, _ *[]model.EquipmentLoad) {
				*zones = append(*zones, model.ThermalZone{ID: 2, ZoneCode: "Z2", Name: "Zone 2", ZoneStatus: "active"})
			},
			wantChanges: 1, wantEntity: EntityZone, wantType: ChangeAdded,
		},
		{
			name: "zone removed",
			mutate: func(_ *Inputs, zones *[]model.ThermalZone, _ *[]model.Rack, _ *[]model.EquipmentLoad) {
				*zones = []model.ThermalZone{}
			},
			wantChanges: 1, wantEntity: EntityZone, wantType: ChangeRemoved,
		},
		{
			name: "rack power and status modified",
			mutate: func(_ *Inputs, _ *[]model.ThermalZone, racks *[]model.Rack, _ *[]model.EquipmentLoad) {
				(*racks)[0].PowerLimitKW = 8
				(*racks)[0].RackStatus = constants.RackMaintenance
			},
			wantChanges: 1, wantEntity: EntityRack, wantType: ChangeModified,
			wantFields: []string{"power_limit_kw", "rack_status"},
		},
		{
			name: "rack moved",
			mutate: func(_ *Inputs, _ *[]model.ThermalZone, racks *[]model.Rack, _ *[]model.EquipmentLoad) {
				(*racks)[0].RowIndex = 2
			},
			wantChanges: 1, wantEntity: EntityRack, wantFields: []string{"row_index"},
		},
		{
			name: "rack added",
			mutate: func(_ *Inputs, _ *[]model.ThermalZone, racks *[]model.Rack, _ *[]model.EquipmentLoad) {
				*racks = append(*racks, model.Rack{ID: 2, RackCode: "R2"})
			},
			wantChanges: 1, wantEntity: EntityRack, wantType: ChangeAdded,
		},
		{
			name: "rack removed",
			mutate: func(_ *Inputs, _ *[]model.ThermalZone, racks *[]model.Rack, _ *[]model.EquipmentLoad) {
				*racks = []model.Rack{}
			},
			wantChanges: 1, wantEntity: EntityRack, wantType: ChangeRemoved,
		},
		{
			name: "selected load power modified",
			mutate: func(_ *Inputs, _ *[]model.ThermalZone, _ *[]model.Rack, loads *[]model.EquipmentLoad) {
				(*loads)[0].PowerKW = 5
			},
			wantChanges: 1, wantEntity: EntityLoad, wantType: ChangeModified, wantFields: []string{"power_kw"},
		},
		{
			name: "selected load status changed",
			mutate: func(_ *Inputs, _ *[]model.ThermalZone, _ *[]model.Rack, loads *[]model.EquipmentLoad) {
				(*loads)[1].LoadStatus = "held"
			},
			wantChanges: 1, wantEntity: EntityLoad, wantFields: []string{"load_status"},
		},
		{
			name: "selected load removed",
			mutate: func(_ *Inputs, _ *[]model.ThermalZone, _ *[]model.Rack, loads *[]model.EquipmentLoad) {
				*loads = (*loads)[:1]
			},
			wantChanges: 1, wantEntity: EntityLoad, wantType: ChangeRemoved,
		},
		{
			name: "unrelated new load is not an input change",
			mutate: func(_ *Inputs, _ *[]model.ThermalZone, _ *[]model.Rack, loads *[]model.EquipmentLoad) {
				*loads = append(*loads, model.EquipmentLoad{ID: 9, Name: "new-load", LoadStatus: "ready"})
			},
			wantChanges: 0,
		},
		{
			name: "algorithm version changed",
			mutate: func(snap *Inputs, _ *[]model.ThermalZone, _ *[]model.Rack, _ *[]model.EquipmentLoad) {
				snap.AlgorithmVersion = "thermal-v0"
			},
			wantChanges: 1, wantEntity: EntityAlgorithm, wantType: ChangeModified,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := baseInputs()
			zones := append([]model.ThermalZone(nil), snap.Zones...)
			racks := append([]model.Rack(nil), snap.Racks...)
			loads := append([]model.EquipmentLoad(nil), snap.Loads...)
			tc.mutate(&snap, &zones, &racks, &loads)
			algorithm := "thermal-v1"
			if tc.wantAlgorithm != "" {
				algorithm = tc.wantAlgorithm
			}
			changes := Diff(snap, zones, racks, loads, algorithm)
			if len(changes) != tc.wantChanges {
				t.Fatalf("got %d changes %v, want %d", len(changes), changes, tc.wantChanges)
			}
			if tc.wantChanges == 0 {
				return
			}
			match := false
			for _, change := range changes {
				if change.EntityType == tc.wantEntity && (tc.wantType == "" || change.ChangeType == tc.wantType) {
					match = true
					if tc.wantFields != nil && !equalStrings(change.Fields, tc.wantFields) {
						t.Fatalf("got fields %v, want %v", change.Fields, tc.wantFields)
					}
					if change.Description == "" {
						t.Fatal("change description must not be empty")
					}
				}
			}
			if !match {
				t.Fatalf("no change matched entity=%s type=%s in %v", tc.wantEntity, tc.wantType, changes)
			}
		})
	}
}

func TestDiffIsSortedAndDeterministic(t *testing.T) {
	snap := baseInputs()
	zones := []model.ThermalZone{}
	racks := append([]model.Rack(nil), snap.Racks...)
	loads := append([]model.EquipmentLoad(nil), snap.Loads...)
	first := Diff(snap, zones, racks, loads, "thermal-v1")
	second := Diff(snap, zones, racks, loads, "thermal-v1")
	if len(first) != 1 || first[0].EntityType != EntityZone {
		t.Fatalf("unexpected changes: %v", first)
	}
	if first[0].Description != second[0].Description {
		t.Fatal("diff is not deterministic")
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	inputs := baseInputs()
	raw, err := Encode(inputs)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !decoded.IsEvaluation() {
		t.Fatal("snapshot should be detected as an evaluation snapshot")
	}
	if len(decoded.Zones) != 1 || len(decoded.Racks) != 1 || len(decoded.Loads) != 2 {
		t.Fatalf("decoded snapshot lost entities: %+v", decoded)
	}
}

func TestDraftSnapshotNotEvaluated(t *testing.T) {
	raw, err := Encode(Inputs{Kind: KindDraft, LoadIDs: []uint{1}, AlgorithmVersion: "thermal-v1"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.IsEvaluation() {
		t.Fatal("draft snapshot must not count as an evaluation snapshot")
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

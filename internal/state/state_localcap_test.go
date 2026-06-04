// internal/state/state_localcap_test.go — tests for local capacity state methods.
package state

import (
	"testing"
	"time"
)

func TestSetLocalCapacity_UpdatesFields(t *testing.T) {
	bs := New("test-backend")

	bs.SetLocalCapacity(3, 4, 0.75, true)

	snap := bs.Snapshot()
	if snap.LocalSlotsIdle != 3 {
		t.Errorf("LocalSlotsIdle = %d, want 3", snap.LocalSlotsIdle)
	}
	if snap.LocalSlotsTotal != 4 {
		t.Errorf("LocalSlotsTotal = %d, want 4", snap.LocalSlotsTotal)
	}
	if snap.LocalVRAMHeadroomPct != 0.75 {
		t.Errorf("LocalVRAMHeadroomPct = %f, want 0.75", snap.LocalVRAMHeadroomPct)
	}
	if !snap.LocalModelHot {
		t.Errorf("LocalModelHot = false, want true")
	}
	if snap.LocalCapacityUpdatedAt.IsZero() {
		t.Errorf("LocalCapacityUpdatedAt should not be zero after SetLocalCapacity")
	}
}

func TestSetLocalCapacity_SnapshotReflectsLatestValues(t *testing.T) {
	bs := New("test-backend")

	bs.SetLocalCapacity(4, 4, 0.9, true)
	bs.SetLocalCapacity(0, 4, 0.5, false) // overwrite

	snap := bs.Snapshot()
	if snap.LocalSlotsIdle != 0 {
		t.Errorf("LocalSlotsIdle = %d, want 0 after second call", snap.LocalSlotsIdle)
	}
	if snap.LocalModelHot {
		t.Errorf("LocalModelHot = true, want false after second call")
	}
	if snap.LocalVRAMHeadroomPct != 0.5 {
		t.Errorf("LocalVRAMHeadroomPct = %f, want 0.5 after second call", snap.LocalVRAMHeadroomPct)
	}
}

func TestSetLocalCapacity_UnknownVRAM(t *testing.T) {
	bs := New("test-backend")
	bs.SetLocalCapacity(2, 4, -1.0, true) // -1 = VRAM unknown

	snap := bs.Snapshot()
	if snap.LocalVRAMHeadroomPct != -1.0 {
		t.Errorf("LocalVRAMHeadroomPct = %f, want -1.0 for unknown VRAM", snap.LocalVRAMHeadroomPct)
	}
}

func TestLocalCapacityFields_EphemeralNotRestoredFromSnapshot(t *testing.T) {
	// LocalCapacity fields are ephemeral — RestoreFromSnapshot does not restore them.
	// Stale slot/VRAM data at daemon restart is worse than no data.
	bs := New("test-backend")
	bs.SetLocalCapacity(2, 4, 0.5, true)

	snap := bs.Snapshot()

	// Verify snapshot captured the values
	if snap.LocalSlotsIdle != 2 {
		t.Fatalf("pre-restore: LocalSlotsIdle = %d, want 2", snap.LocalSlotsIdle)
	}

	// Restore into a fresh BackendState
	bs2 := New("test-backend")
	bs2.RestoreFromSnapshot(snap)

	snap2 := bs2.Snapshot()
	if snap2.LocalSlotsIdle != 0 {
		t.Errorf("after RestoreFromSnapshot: LocalSlotsIdle = %d, want 0 (ephemeral)", snap2.LocalSlotsIdle)
	}
	if snap2.LocalSlotsTotal != 0 {
		t.Errorf("after RestoreFromSnapshot: LocalSlotsTotal = %d, want 0 (ephemeral)", snap2.LocalSlotsTotal)
	}
	if snap2.LocalModelHot {
		t.Errorf("after RestoreFromSnapshot: LocalModelHot = true, want false (ephemeral)")
	}
	if !snap2.LocalCapacityUpdatedAt.IsZero() {
		t.Errorf("after RestoreFromSnapshot: LocalCapacityUpdatedAt = %v, want zero (ephemeral)", snap2.LocalCapacityUpdatedAt)
	}
}

func TestSetLocalCapacity_TimestampAdvances(t *testing.T) {
	bs := New("test-backend")

	before := time.Now().UTC()
	bs.SetLocalCapacity(1, 2, 0.5, true)
	after := time.Now().UTC()

	snap := bs.Snapshot()
	if snap.LocalCapacityUpdatedAt.Before(before) || snap.LocalCapacityUpdatedAt.After(after) {
		t.Errorf("LocalCapacityUpdatedAt %v not between %v and %v", snap.LocalCapacityUpdatedAt, before, after)
	}
}

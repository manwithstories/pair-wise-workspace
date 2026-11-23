package index

import "testing"

func TestSyncTableRoundTrip(t *testing.T) {
	_, fixed := fixture(20)
	units, _ := fixture(20)
	idx, _ := Rebuild(units, fixed)
	data, err := ExportSyncTable(idx, 3600)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("empty export")
	}
	nums, err := ReadSyncTable(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(nums) != len(idx.Entries) {
		t.Fatalf("sample numbers=%v, want %d entries", nums, len(idx.Entries))
	}
	for i, e := range idx.Entries {
		if nums[i] != uint32(e.Seq)+1 {
			t.Fatalf("entry %d: got %d want %d", i, nums[i], e.Seq+1)
		}
	}
	// Deterministic: exporting twice yields identical bytes.
	again, _ := ExportSyncTable(idx, 3600)
	if string(again) != string(data) {
		t.Fatal("export not deterministic")
	}
}

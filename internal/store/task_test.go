package store_test

import (
	"testing"

	"mediascan/internal/store"
)

func TestTaskKeyUsesAssetVersionAndParamsOnly(t *testing.T) {
	params := store.Params{Workers: 16, Seed: 17}
	a, err := store.NewTask(store.JobID("/volume/a/file.mp4"), "file.mp4", "asset-hash", params)
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.NewTask(store.JobID("/volume/b/renamed.mov"), "renamed.mov", "asset-hash", params)
	if err != nil {
		t.Fatal(err)
	}
	if a.Key != b.Key {
		t.Fatalf("same asset/version/params produced different keys: %s != %s", a.Key, b.Key)
	}

	other, err := store.NewTask(b.JobID, b.AssetName, "changed-hash", params)
	if err != nil {
		t.Fatal(err)
	}
	if other.Key == a.Key {
		t.Fatal("changed asset hash must change task key")
	}
}

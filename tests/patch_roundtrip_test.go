package tests

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"example.com/gkpack/internal/delta"
	"example.com/gkpack/internal/pack"
	"example.com/gkpack/internal/store"
	"example.com/gkpack/internal/verify"
	"example.com/gkpack/internal/versiondb"
)

func TestForwardReverseRoundTrip(t *testing.T) {
	root := t.TempDir()
	v1dir := filepath.Join(root, "v1")
	v2dir := filepath.Join(root, "v2")
	v1files := map[string]string{
		"assets/a.txt":         strings.Repeat("alpha beta gamma delta\n", 2200),
		"assets/audio/a.ogg":   strings.Repeat("OggS-sample-pattern-", 2500),
		"assets/tex/one.ktx":   strings.Repeat("KTX1-texture-plane-", 1800),
		"assets/tex/two.ktx":   strings.Repeat("KTX1-texture-plane-", 1800),
		"empty.bin":            "",
		"docs/中文 名称.txt":       strings.Repeat("内容稳定且可压缩\n", 900),
		"assets/delete-me.dat": strings.Repeat("delete this resource\n", 500),
	}
	writeTree(t, v1dir, v1files)
	writeTree(t, v2dir, v1files)

	// Modify one asset, remove one, add one. Duplicate content keeps testing
	// cross-file deduplication.
	if err := os.WriteFile(filepath.Join(v2dir, "assets/a.txt"), []byte(strings.Repeat("alpha beta gamma epsilon\n", 2300)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(v2dir, "assets/delete-me.dat")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v2dir, "assets/new.bin"), []byte(strings.Repeat("new nightly block data\n", 1600)), 0o644); err != nil {
		t.Fatal(err)
	}
	// Symbolic links must be ignored by the scanner.
	if err := os.Symlink(filepath.Join(v1dir, "assets/a.txt"), filepath.Join(v1dir, "assets/link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(v2dir, "assets/a.txt"), filepath.Join(v2dir, "assets/link.txt")); err != nil {
		t.Fatal(err)
	}

	base := buildArchive(t, v1dir, filepath.Join(root, "v1.gkpack"), "v1", 2)
	target := buildArchive(t, v2dir, filepath.Join(root, "v2.gkpack"), "v2", 2)
	patchPath := filepath.Join(root, "v1-v2.gkpatch")
	created, err := delta.Create(delta.CreateOptions{BaseArchive: base.archive, TargetArchive: target.archive, PatchPath: patchPath})
	if err != nil {
		t.Fatal(err)
	}
	if created.ChangedBlocks == 0 || created.ReverseChunks == 0 {
		t.Fatalf("patch should carry forward and reverse blocks: %+v", created)
	}

	upgraded := filepath.Join(root, "upgraded")
	applied, err := delta.Apply(delta.ApplyOptions{
		BaseArchive: base.archive,
		PatchPath:   patchPath,
		OutputDir:   upgraded,
	})
	if err != nil {
		t.Fatalf("forward apply: %v", err)
	}
	if applied.Root != target.root {
		t.Fatalf("forward root %s, want %s", applied.Root, target.root)
	}
	checkTreeResult(t, target.archive, upgraded)

	if _, err := delta.Apply(delta.ApplyOptions{
		BaseArchive: target.archive, // wrong base for the forward direction
		PatchPath:   patchPath,
		OutputDir:   filepath.Join(root, "wrong-base"),
	}); err == nil {
		t.Fatal("apply should reject a baseline root mismatch")
	}

	rolledBack := filepath.Join(root, "rolled-back")
	reverted, err := delta.Apply(delta.ApplyOptions{
		BaseArchive: target.archive,
		PatchPath:   patchPath,
		OutputDir:   rolledBack,
		Reverse:     true,
	})
	if err != nil {
		t.Fatalf("reverse apply: %v", err)
	}
	if reverted.Root != base.root {
		t.Fatalf("reverse root %s, want %s", reverted.Root, base.root)
	}
	if reverted.Restored != 1 || reverted.Removed != 1 {
		t.Fatalf("unexpected rollback file counts: restored=%d removed=%d", reverted.Restored, reverted.Removed)
	}
	checkTreeResult(t, base.archive, rolledBack)

	if _, err := os.Stat(filepath.Join(rolledBack, "empty.bin")); err != nil {
		t.Fatalf("empty file was not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rolledBack, "docs", "中文 名称.txt")); err != nil {
		t.Fatalf("UTF-8 path with space was not restored: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(rolledBack, "assets", "link.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink should not be reconstructed: %v", err)
	}
}

func TestMultiHopAndCrossBranch(t *testing.T) {
	root := t.TempDir()
	v1dir := filepath.Join(root, "v1")
	v2dir := filepath.Join(root, "v2")
	v3dir := filepath.Join(root, "v3")
	v4dir := filepath.Join(root, "v4")
	for _, dir := range []string{v1dir, v2dir, v3dir, v4dir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	base := strings.Repeat("level payload line\n", 1000)
	writeFile(t, filepath.Join(v1dir, "assets/level.dat"), base)
	writeFile(t, filepath.Join(v2dir, "assets/level.dat"), base+"v2 change\n")
	writeFile(t, filepath.Join(v3dir, "assets/level.dat"), base+"v2 change\nv3 detail\n")
	writeFile(t, filepath.Join(v4dir, "assets/level.dat"), base+"v2 change\n")
	writeFile(t, filepath.Join(v4dir, "assets/dev-only.bin"), strings.Repeat("branch data\n", 900))

	a1 := buildArchive(t, v1dir, filepath.Join(root, "v1.gkpack"), "v1", 2)
	a2 := buildArchive(t, v2dir, filepath.Join(root, "v2.gkpack"), "v2", 2)
	a3 := buildArchive(t, v3dir, filepath.Join(root, "v3.gkpack"), "v3", 2)
	a4 := buildArchive(t, v4dir, filepath.Join(root, "v4.gkpack"), "v4", 2)

	p12 := makePatch(t, a1.archive, a2.archive, filepath.Join(root, "p12.gkpatch"))
	p23 := makePatch(t, a2.archive, a3.archive, filepath.Join(root, "p23.gkpatch"))
	p24 := makePatch(t, a2.archive, a4.archive, filepath.Join(root, "p24.gkpatch"))

	dbdir := filepath.Join(root, "db")
	db, err := versiondb.Open(dbdir)
	if err != nil {
		t.Fatal(err)
	}
	versions := []versiondb.Version{
		{Name: "v1", Branch: "main", Archive: a1.archive},
		{Name: "v2", Branch: "stable", Parent: "v1", Archive: a2.archive},
		{Name: "v3", Branch: "stable", Parent: "v2", Archive: a3.archive},
		{Name: "v4", Branch: "dev", Parent: "v2", Archive: a4.archive},
	}
	for _, v := range versions {
		if err := db.AddVersion(v); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AddPatch("v1", "v2", p12); err != nil {
		t.Fatal(err)
	}
	if err := db.AddPatch("v2", "v3", p23); err != nil {
		t.Fatal(err)
	}
	if err := db.AddPatch("v2", "v4", p24); err != nil {
		t.Fatal(err)
	}

	hops, err := db.Path("v1", "v3")
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 2 {
		t.Fatalf("v1->v3 hops = %d, want 2", len(hops))
	}
	out := filepath.Join(root, "multi")
	assertPathApply(t, db, hops, out, a3.root)
	checkTreeResult(t, a3.archive, out)

	// v3 -> v4 crosses branches through their common ancestor v2.
	hops, err = db.Path("v3", "v4")
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 2 || !hops[0].Reverse {
		t.Fatalf("v3->v4 should reverse to v2 then move to dev: %+v", hops)
	}
	out2 := filepath.Join(root, "cross-branch")
	assertPathApply(t, db, hops, out2, a4.root)
	checkTreeResult(t, a4.archive, out2)

	// Descendant rollback to an ancestor uses reverse hops.
	hops, err = db.Path("v3", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 2 || !hops[0].Reverse || !hops[1].Reverse {
		t.Fatalf("v3->v1 should be two reverse hops: %+v", hops)
	}
	out3 := filepath.Join(root, "descendant-rollback")
	assertPathApply(t, db, hops, out3, a1.root)
	checkTreeResult(t, a1.archive, out3)

	// A non-empty unmatched version is unreachable.
	writeFile(t, filepath.Join(root, "isolated", "x"), "x")
	a5 := buildArchive(t, filepath.Join(root, "isolated"), filepath.Join(root, "v5.gkpack"), "v5", 1)
	if err := db.AddVersion(versiondb.Version{Name: "v5", Branch: "experiment", Archive: a5.archive}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Path("v1", "v5"); err == nil {
		t.Fatal("unreachable version should fail")
	}
}

func TestDeterministicConcurrentBuild(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	for i := 0; i < 48; i++ {
		name := filepath.Join(src, "assets", "file_"+pad(i)+".bin")
		writeFile(t, name, strings.Repeat("deterministic-block-"+pad(i)+"\n", 700+i*3))
	}
	writeFile(t, filepath.Join(src, "assets", "duplicate-a.bin"), strings.Repeat("same content\n", 3000))
	writeFile(t, filepath.Join(src, "assets", "duplicate-b.bin"), strings.Repeat("same content\n", 3000))

	previousProcs := runtime.GOMAXPROCS(1)
	a := buildArchive(t, src, filepath.Join(root, "one.gkpack"), "stable", 1)
	runtime.GOMAXPROCS(4)
	b := buildArchive(t, src, filepath.Join(root, "many.gkpack"), "stable", 8)
	runtime.GOMAXPROCS(previousProcs)
	one, err := os.ReadFile(a.archive)
	if err != nil {
		t.Fatal(err)
	}
	many, err := os.ReadFile(b.archive)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(one, many) {
		t.Fatal("archive bytes differ across worker counts")
	}
	if a.root != b.root {
		t.Fatalf("roots differ: %s vs %s", a.root, b.root)
	}

	target := filepath.Join(root, "target")
	writeFile(t, filepath.Join(target, "assets", "new.bin"), strings.Repeat("target-only block\n", 900))
	for i := 0; i < 12; i++ {
		writeFile(t, filepath.Join(target, "assets", "file_"+pad(i)+".bin"), strings.Repeat("deterministic-block-"+pad(i)+"\n", 700+i*3))
	}
	t1 := buildArchive(t, target, filepath.Join(root, "target-one.gkpack"), "target", 1)
	t8 := buildArchive(t, target, filepath.Join(root, "target-many.gkpack"), "target", 8)
	p1 := makePatch(t, a.archive, t1.archive, filepath.Join(root, "one.gkpatch"))
	p8 := makePatch(t, b.archive, t8.archive, filepath.Join(root, "many.gkpatch"))
	patchOne, err := os.ReadFile(p1)
	if err != nil {
		t.Fatal(err)
	}
	patchMany, err := os.ReadFile(p8)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(patchOne, patchMany) {
		t.Fatal("patch bytes differ across worker counts")
	}
}

func TestVerifyFindsAllCorruptPathsAndArchiveDamage(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	writeFile(t, filepath.Join(src, "assets/tex/hero_02.ktx"), strings.Repeat("hero texture data\n", 3000))
	writeFile(t, filepath.Join(src, "assets/audio/bgm_03.ogg"), strings.Repeat("audio sample data\n", 3000))
	writeFile(t, filepath.Join(src, "assets/keep.dat"), strings.Repeat("safe data\n", 3000))
	archive := buildArchive(t, src, filepath.Join(root, "v1.gkpack"), "v1", 2)
	applyBaseTree(t, archive.archive, filepath.Join(root, "tree"))
	tree := filepath.Join(root, "tree")

	for _, name := range []string{"assets/tex/hero_02.ktx", "assets/audio/bgm_03.ogg"} {
		path := filepath.Join(tree, filepath.FromSlash(name))
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data[1] ^= 0x40
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := verify.Both(archive.archive, tree)
	if err != nil {
		t.Fatal(err)
	}
	if res.FailedFiles != 2 {
		t.Fatalf("failed files = %d, want 2: %+v", res.FailedFiles, res.Paths)
	}
	want := []string{"assets/audio/bgm_03.ogg", "assets/tex/hero_02.ktx"}
	if !reflect.DeepEqual(res.Paths, want) {
		t.Fatalf("paths = %v, want %v", res.Paths, want)
	}
	if res.FailedBlocks != 2 {
		t.Fatalf("failed blocks = %d, want 2", res.FailedBlocks)
	}

	damaged := filepath.Join(root, "damaged.gkpack")
	data, err := os.ReadFile(archive.archive)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-3] ^= 0x01
	if err := os.WriteFile(damaged, data, 0o644); err != nil {
		t.Fatal(err)
	}
	damageResult, err := verify.Archive(damaged)
	if err != nil {
		t.Fatal(err)
	}
	if damageResult.FailedBlocks == 0 || damageResult.FailedFiles == 0 {
		t.Fatalf("payload flip not detected: %+v", damageResult)
	}

	truncated := filepath.Join(root, "truncated.gkpack")
	if err := os.WriteFile(truncated, data[:len(data)-8], 0o644); err != nil {
		t.Fatal(err)
	}
	truncResult, err := verify.Archive(truncated)
	if err != nil {
		t.Fatal(err)
	}
	if truncResult.FailedBlocks == 0 || truncResult.FailedFiles == 0 {
		t.Fatalf("truncation not detected: %+v", truncResult)
	}

	tampered := filepath.Join(root, "tampered.gkpack")
	tamperData, err := os.ReadFile(archive.archive)
	if err != nil {
		t.Fatal(err)
	}
	tamperData[50] ^= 0x01
	if err := os.WriteFile(tampered, tamperData, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := verify.Archive(tampered); err == nil {
		t.Fatal("manifest/header tampering should fail")
	}
}

func TestApplyResumesCompleteFilesAndCheckpoint(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	writeFile(t, filepath.Join(src, "a.txt"), strings.Repeat("alpha\n", 2100))
	writeFile(t, filepath.Join(src, "b.txt"), strings.Repeat("bravo\n", 2100))
	a := buildArchive(t, src, filepath.Join(root, "v1.gkpack"), "v1", 2)

	modified := filepath.Join(root, "src2")
	writeFile(t, filepath.Join(modified, "a.txt"), strings.Repeat("alpha\n", 2100)+strings.Repeat("extra\n", 500))
	writeFile(t, filepath.Join(modified, "b.txt"), strings.Repeat("bravo\n", 2100))
	writeFile(t, filepath.Join(modified, "c.txt"), strings.Repeat("charlie\n", 2100))
	b := buildArchive(t, modified, filepath.Join(root, "v2.gkpack"), "v2", 2)
	patchPath := makePatch(t, a.archive, b.archive, filepath.Join(root, "p.gkpatch"))

	complete := filepath.Join(root, "complete")
	if _, err := delta.Apply(delta.ApplyOptions{BaseArchive: a.archive, PatchPath: patchPath, OutputDir: complete}); err != nil {
		t.Fatal(err)
	}
	reader, err := delta.Open(patchPath)
	if err != nil {
		t.Fatal(err)
	}
	targetManifest := reader.ManifestFor(false)
	first := targetManifest.Files[0]
	_ = reader.Close()

	partial := filepath.Join(root, "partial")
	if err := os.MkdirAll(partial, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(complete, filepath.FromSlash(first.Path)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, filepath.FromSlash(first.Path)), data, 0o644); err != nil {
		t.Fatal(err)
	}
	checkpointData, err := json.Marshal(map[string]string{"root": targetManifest.Root, "path": first.Path})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, ".gkpack-apply.log"), append(checkpointData, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	resumed, err := delta.Apply(delta.ApplyOptions{BaseArchive: a.archive, PatchPath: patchPath, OutputDir: partial})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ResumedFiles < 1 {
		t.Fatalf("expected at least one resumed file, got %+v", resumed)
	}
	checkTreeResult(t, b.archive, partial)

	again, err := delta.Apply(delta.ApplyOptions{BaseArchive: a.archive, PatchPath: patchPath, OutputDir: complete})
	if err != nil {
		t.Fatal(err)
	}
	if again.ResumedFiles != len(targetManifest.Files) {
		t.Fatalf("second apply resumed %d files, want %d", again.ResumedFiles, len(targetManifest.Files))
	}
	if _, err := os.Stat(filepath.Join(complete, ".gkpack-apply.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("checkpoint should be removed after success: %v", err)
	}
}

func TestStoreDedupRefsAndGC(t *testing.T) {
	st, cleanup, err := store.NewTemp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	data := []byte(strings.Repeat("shared block\n", 100))
	first, err := st.Put(data)
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.Put(data)
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash != second.Hash || st.Refs(first.Hash) != 2 {
		t.Fatalf("dedup/refcount failed: %+v %+v refs=%d", first, second, st.Refs(first.Hash))
	}
	if _, err := st.GC(map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if len(st.Entries()) != 0 {
		t.Fatal("GC should remove unreferenced blocks")
	}
}

type builtArchive struct {
	archive string
	root    string
}

func buildArchive(t *testing.T, src, archive, version string, workers int) builtArchive {
	t.Helper()
	res, err := pack.Build(pack.BuildOptions{SourceDir: src, ArchivePath: archive, Version: version, Workers: workers})
	if err != nil {
		t.Fatalf("build %s: %v", version, err)
	}
	return builtArchive{archive: archive, root: res.Root}
}

func makePatch(t *testing.T, base, target, patch string) string {
	t.Helper()
	if _, err := delta.Create(delta.CreateOptions{BaseArchive: base, TargetArchive: target, PatchPath: patch}); err != nil {
		t.Fatalf("create patch: %v", err)
	}
	return patch
}

func assertPathApply(t *testing.T, db *versiondb.DB, hops []versiondb.Hop, out, wantRoot string) {
	t.Helper()
	for i, hop := range hops {
		base := db.Versions[hop.From]
		res, err := delta.Apply(delta.ApplyOptions{
			BaseArchive:   base.Archive,
			PatchPath:     hop.Patch,
			OutputDir:     out,
			Reverse:       hop.Reverse,
			ExpectVersion: hop.To,
		})
		if err != nil {
			t.Fatalf("hop %d %s->%s: %v", i+1, hop.From, hop.To, err)
		}
		if res.Root != db.Versions[hop.To].Root {
			t.Fatalf("hop %d root %s, want %s", i+1, res.Root, db.Versions[hop.To].Root)
		}
	}
	actual, err := pack.ScanTree(out)
	if err != nil {
		t.Fatal(err)
	}
	if actual.Root != wantRoot {
		t.Fatalf("path result root %s, want %s", actual.Root, wantRoot)
	}
}

func checkTreeResult(t *testing.T, archive, tree string) {
	t.Helper()
	res, err := verify.Both(archive, tree)
	if err != nil {
		t.Fatal(err)
	}
	if res.FailedFiles != 0 {
		t.Fatalf("verification failed: %+v", res.Issues)
	}
}

func applyBaseTree(t *testing.T, archive, out string) {
	t.Helper()
	reader, err := pack.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	m := reader.Manifest()
	for _, f := range m.Files {
		rel, err := pack.CleanRelativePath(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		full := filepath.Join(out, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		var content []byte
		for _, b := range f.Blocks {
			raw, err := reader.ReadChunk(b.Hash)
			if err != nil {
				t.Fatal(err)
			}
			content = append(content, raw...)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		writeFile(t, filepath.Join(root, filepath.FromSlash(name)), files[name])
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func pad(i int) string {
	return fmt.Sprintf("%04d", i)
}

# gkpack

`gkpack` is a fully local, deterministic block-delta packer for game resources.
It builds content-addressed archives, creates bidirectional patches between any
two versions, reconstructs either direction, verifies every block, and tracks a
branch graph for shortest-path multi-hop upgrades and rollbacks.

The implementation uses Go 1.22, `github.com/klauspost/compress/zstd` v1.17.9,
and the standard library. Dependencies are vendored; runtime operations never
touch the network.

## Demo

```sh
bash scripts/demo.sh
```

The generated fixture has 3072 deterministic text/binary resources. The script
demonstrates build, forward delta, upgrade, rollback, bit-flip injection, and
verification. All temporary files are created under a temporary directory.

## Commands

```sh
# Build a version archive. Workers only affect speed, never bytes.
go run ./cmd/gkpack build --src resources --archive v1.gkpack --version v1

# Create one patch containing both v1->v2 and v2->v1 directions.
go run ./cmd/gkpack delta --base v1.gkpack --target v2.gkpack --patch v1-v2.gkpatch --verbose

# Upgrade into a fresh tree.
go run ./cmd/gkpack apply --base v1.gkpack --patch v1-v2.gkpatch --out restored

# Roll back using the embedded reverse direction.
go run ./cmd/gkpack apply --base v2.gkpack --patch v1-v2.gkpatch --out rollback --reverse

# Verify an archive, an extracted tree, or both.
go run ./cmd/gkpack verify --archive v2.gkpack
go run ./cmd/gkpack verify --archive v2.gkpack --tree restored
```

## Version graph and multi-hop paths

```sh
go run ./cmd/gkpack version add --db versions --name v1 --branch main --archive v1.gkpack
go run ./cmd/gkpack version add --db versions --name v2 --branch stable --parent v1 --archive v2.gkpack
go run ./cmd/gkpack version patch --db versions --from v1 --to v2 --patch v1-v2.gkpatch
go run ./cmd/gkpack version path --db versions --from v1 --to v2
go run ./cmd/gkpack apply-path --db versions --from v1 --to v2 --out restored
```

Patches are bidirectional, so the graph is traversed as undirected edges and the
CLI automatically chooses the reverse side when needed. `apply-path` rebuilds
and re-verifies the target root at every hop.

## Formats and guarantees

- Chunking uses an in-house buzhash rolling hash with 4 KiB minimum and 128 KiB
  maximum chunk sizes. Equal content produces equal hashes and is shared.
- Each archive has a sealed JSON header, a version dictionary, a sorted chunk
  index, and a canonical manifest. The manifest root is a SHA-256 over sorted
  paths, sizes, file hashes, block hashes, offsets, and block sizes.
- Every block is checked independently against its SHA-256 when read. Files are
  written to a sibling temporary file and atomically renamed only after size and
  file SHA-256 match.
- Interrupted applies leave `.gkpack-apply.log` plus complete files. A rerun
  revalidates completed files and resumes from there; the log is removed after
  the final tree root is recomputed successfully.
- Archive and manifest order is independent of `GOMAXPROCS` and worker count.
- Verification continues after damaged blocks/files, reports every affected
  path, and exits non-zero when anything fails.

## Exit codes

- `0`: success
- `1`: operation or integrity precondition failure
- `2`: command-line usage error
- `3`: verification found corrupt, missing, truncated, or tampered data

Errors are emitted to stderr as JSON objects. Verification failures additionally
include per-path JSON records and a final aggregate record.

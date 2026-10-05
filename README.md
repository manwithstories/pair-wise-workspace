# Contract version store

A git-compatible repository for a law firm's contract archive, built for the case
where documents are large, versions differ by a few clauses, and the firm's
existing git toolchain must still be able to audit what was committed.

Three properties drive the design:

1. **Commit hashes are identical to native git's.** Every object is serialized in
   git's canonical byte format, so an audit done with `git log`, `git fsck` or
   JGit sees exactly the objects this store produced.
2. **Large documents are stored incrementally.** A 30 MB contract is split by
   content into Rabin-fingerprinted chunks; a new version is a recipe of
   `COPY` and `INSERT` instructions referencing chunks already on disk, so
   amending one clause costs kilobytes instead of another full copy.
3. **A commit does not copy the tree.** Trees are persistent and copy-on-write:
   editing one path rebuilds only the nodes along that path, and every unchanged
   sibling subtree is shared by reference.

## Verify

```sh
./verify.sh
```

This compiles the sources, runs the JUnit 5 suite (which includes cross-checks
against the system `git` binary and JGit), then replays one add/commit sequence
independently with this VCS and with `git` in a temporary directory and diffs
every object hash and the full tree structure.

Use `./verify.sh --quick` for summary output, and `./build.sh` to compile and run
just the suite.

## Using it

```sh
CP=build/classes

java -cp $CP lawfirm.vcs.Main init /srv/contracts
java -cp $CP lawfirm.vcs.Main add-all /srv/contracts
java -cp $CP lawfirm.vcs.Main commit /srv/contracts -m "2024 renewal round"

java -cp $CP lawfirm.vcs.Main log -n 5 /srv/contracts
java -cp $CP lawfirm.vcs.Main dump-graph /srv/contracts
java -cp $CP lawfirm.vcs.Main checkout /srv/contracts HEAD~1
java -cp $CP lawfirm.vcs.Main cat-file /srv/contracts HEAD
```

`add-all` stages everything except the repository's own `.vcs` directory.
`add <file>`, `rm <path>`, `status`, `hash-object` and `chunk-stats` round out
the set. `GIT_AUTHOR_DATE` / `GIT_COMMITTER_DATE` are honoured, so a scripted
sequence can be replayed reproducibly.

## Layout on disk

```
.vcs/HEAD                 "ref: refs/heads/main"
.vcs/refs/heads/main      current commit id
.vcs/refs/stage           staged root tree id
.vcs/log                  one line per commit
.vcs/objects/xx/yyyy…     deflated loose blobs, trees and commits (git's format)
.vcs/pack/chunks/…        shared chunk pool, addressed by chunk SHA-1
.vcs/pack/recipes/…       copy/insert delta instructions per blob version
```

Blobs up to 1 MB are ordinary loose git objects. Larger ones go through Rabin
chunking and the chunk pack. Either way the blob's logical id is the SHA-1 git
computes over `blob <size>\0<content>`, and reading it back yields those exact
bytes.

## Design notes

**Object identity.** `blob`, `tree` and `commit` are serialized byte for byte as
git writes them: `blob <size>\0<content>`; `<mode> <name>\0<20-byte-id>` per tree
entry; and for commits the header block, a blank line, and a message that always
ends in a newline. Two details are easy to get wrong and both change every id:
tree entries sort byte-wise with a directory compared as `name/`, and a directory
mode is written `40000` while a file is `100644`.

**Chunking.** The fingerprint is the remainder of a 16-byte sliding window modulo
`x^128 + x^7 + x^2 + x + 1`, advanced by an exact rolling update, so a window's
fingerprint depends only on its content and never on where it sits. Boundaries
are therefore content defined: editing one clause resegments only the
neighbourhood of the edit, and the rest of the document keeps the same chunk ids
and can be referenced instead of stored. Measured on contract-like text, a
clause insertion preserves 89–98% of chunks.

**Trees.** Nodes are immutable and interned by content, so equal subtrees are one
object. Inserting a path rebuilds only its ancestors; the store skips any node
whose id is already present, so a 100k-file commit rewrites three or four tree
objects rather than the whole tree. A directory's own name is not part of a tree
object's bytes, which is why a loaded tree keys nodes on their children and keeps
names on the referencing node.

**Delta storage.** A version similar to an earlier one is stored as a recipe of
`COPY` (a run of the base's chunks) and `INSERT` (a chunk id) instructions.
Recipes name their base and are resolved recursively on read with a depth cap;
reconstruction is verified against the blob id, so a corrupt chain fails loudly
instead of returning wrong bytes.

## Measured behaviour

Measured on an Apple Silicon laptop, JDK 24 compiling to Java 17 bytecode:

| Budget | Required | Measured |
| --- | --- | --- |
| add + commit of a small file | < 50 ms | 21 ms + 29 ms (first call, includes JIT) |
| commit over a 100k-file tree | < 500 ms | < 1 ms incremental (284 ms for the initial full write) |
| second version of a 100 MB contract | < 5% of full | 0.027% (28 KB added) |
| read + inflate a 1 MB blob | < 20 ms | ~1 ms |
| retained heap for a 100k-path tree | < 256 MB | 14 MB |

The 100 MB row is the one that matters for the stated problem. Storing the first
version deflated it to 1.0 MB; amending a single clause then added 28 KB. Twenty
successive amendments of a 10.7 MB contract added roughly 120 KB in total, against
the 40 GB the same archive costs under per-commit full blobs.

The small-file timings are dominated by first-call class loading; the
`PerformanceTest` timings take the best of several runs after a warm-up and are
lower still.

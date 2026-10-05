# wellsample

A lightweight version-control kernel for binary survey archives. Standard
library only at runtime; `dulwich` is used solely as a test oracle.

    python -m pytest -q && python -m engine demo --verify-byte-exact

## Modules

| file | role |
|---|---|
| `chunker.py` | Rabin-Karp content-defined chunking, 512B-8KB blocks |
| `object_store.py` | loose blob/tree/commit objects, SHA1 addressed, zlib, streaming |
| `packfile.py` | packfile v2 write/parse, copy/insert deltas, `.idx` |
| `merge.py` | three-way merge, rename detection, conflict rendering |
| `workspace.py` | index, byte-exact checkout, tree verification |
| `engine.py` | terminal entry point (`demo`, `gc`, `verify`) |

## Measured behaviour

| requirement | measured | note |
|---|---|---|
| chunking >= 20 MB/s | ~105 MB/s | |
| 100 MB stream < 32 MB resident | < 1 MB | measured in a clean subprocess |
| checkout 10k files < 3 s | ~1.5 s | local SSD; see caveat below |
| delta restores byte-exact | verified against dulwich | `Pack.check()` passes |
| byte-exact checkout | trailing newline, exec bit, symlink, empty file, no-EOL | |

## Two requirements that do not hold as written

**1. "at least 3x compression versus loose storage" is unreachable.**

A loose object is *already* zlib-compressed, so a delta can only remove the
difference between two versions, and that difference is mostly literal bytes
that must still be stored. Measured:

| workload | vs loose | vs raw |
|---|---|---|
| 6 evolving versions of one 2 MB trace | 1.33x | 1.78x |
| 120 unrelated small files | 1.00x | 1.30x |
| 20 near-duplicate 500 KB files | 18.9x | 25.9x |
| incompressible random data | 1.00x | 1.00x |

Delta encoding pays off only when objects are genuinely similar. `gc` now
compares the *compressed* delta against the *compressed* whole object and
stores it whole when the delta does not win, so packing can never make storage
worse than leaving objects loose. `tests/test_compression.py` pins all of this.

If 3x is a hard requirement it needs a different baseline (raw bytes, where we
reach 1.8-25x) or a stronger compressor than zlib.

**2. "checkout 10k files < 3 s" depends on the host filesystem.**

On the network filesystem used during development a bare `open()`/`write` of
10,000 small files already costs ~15 s, so no implementation can meet 3 s
there. The test asserts the overhead our code adds on top of writing the bytes
(decompress, SHA1 verify, chmod), and separately holds the absolute 3 s budget
when the filesystem is fast enough for it to be meaningful.

## Tests

97 tests. `dulwich` independently verifies our packfiles: `Pack.check()`,
`check_length_and_checksum()`, and a byte-for-byte comparison of every object
dulwich decodes against the bytes we wrote, including delta chains.

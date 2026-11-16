# lutgrade

Offline 3D LUT validation and rendering for game-engine / offline-render
colour pipelines. Pure C++17, no graphics window, no network: it reads
controlled PPM images and self-parsed `.cube` LUTs, applies a colour-space
matrix and a trilinear LUT, and reports enough statistics to tell a correct
grade from a mis-parsed one.

## Build

```bash
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j2
```

Dependencies are vendored under `.third_party/` (CLI11 2.4.2, Catch2 3.5.2),
so configuring works with no network access.

## Demo

```bash
./build/lutgrade generate-fixture --dir fixtures --size 33
./build/lutgrade run --input fixtures/input.ppm --lut fixtures/demo.cube \
  --in-space srgb --out-space rec709 --path tiled --threads 4 \
  --output /tmp/out.ppm
./build/lutgrade compare --input fixtures/input.ppm --lut fixtures/demo.cube \
  --in-space srgb --out-space rec709 --threads 4
```

`scripts/make_fixtures.sh` runs all of the above plus the test suite.

Expected shape of `run` output (values from the shipped demo at 33³):

```
STAGE=decode:srgb-eotf matrix:srgb->rec709 encode:bt709-eotf lut:trilinear
GRID_SIZE=33
PIXELS=4096
PATH=tiled
THREADS=4
IN_SPACE=srgb
OUT_SPACE=rec709
MAX_DEV_R=0.000076890
MAX_DEV_G=0.000077009
MAX_DEV_B=0.000075579
MEAN_DEV=0.000045763
HIST_BEFORE_R=...   (256 comma-separated bins)
...
HIST_AFTER_B=...
SATURATED_PIXELS=0
HASH=e4047e28cd1fc13b
OUTPUT=/tmp/out.ppm
```

`compare` reports `COMPARE_RESULT=MATCH|MISMATCH`, `MAX_DIFF`, the first
differing coordinate/channel/values, and both hashes.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 2 | parameter or input-format error (bad flags, PPM, `.cube`) |
| 3 | unknown colour space, or a non-finite matrix entry |
| 4 | render failure (non-finite pixel, malformed table mid-render) |
| 5 | scalar and tiled paths differ beyond `--threshold` |

## Design

Pipeline, per pixel, in one shared kernel (`src/interpolator.cpp`):

```
8-bit sample → decode source transfer → 3x3 matrix in linear light
→ clamp + flag out-of-gamut → encode destination transfer
→ trilinear LUT sample (domain-mapped, edge-clamped) → clamp + flag
```

Both render paths call that kernel and nothing else; parallelism only changes
scheduling, never arithmetic.

| Module | Responsibility |
|---|---|
| `src/image_io.*` | strict P6 PPM read/write, exact raster size |
| `src/lut_parser.*` | `.cube` directives, 17/33/65 grids, red-fastest order |
| `src/color_matrix.*` | sRGB / Rec.709 / Rec.2020 / ACEScg matrices + transfer functions |
| `src/interpolator.*` | the shared kernel and trilinear sampler |
| `src/renderer_scalar.*` | single-threaded reference path |
| `src/renderer_tiled.*` | fixed 32×32 tiles, atomic work queue, deterministic abort |
| `src/statistics.*` | histograms, deviation, saturation, FNV-1a hash |
| `src/fixture.*` | deterministic PPM and `.cube` generation |
| `src/cli.*`, `src/main.cpp`, `src/commands.*` | argument validation and subcommands |

### Correctness properties checked by the tests

- Scalar and tiled output are byte-identical at 1, 2, 3, 4 and 8 threads.
- The tiled path preserves row-major pixel ordering across tile boundaries.
- A failing tile aborts the whole pass with exit 4; no output is written.
- `.cube` parsing rejects missing/duplicate directives, directives after data,
  unsupported sizes, non-finite values, values outside `[0,1]`, short or
  surplus data, and inverted domains.
- Non-finite pixels raise a render error naming the coordinate rather than
  leaking into the statistics.
- Fixture generation is deterministic, so hashes are stable across runs.

## Layout note

The demo LUT is a mild S-curve rather than a strong grade: the reported
`MAX_DEV`/`MEAN_DEV` measure the LUT's own pre→post contribution, so a gentle
curve keeps them in the sub-1e-4 band a healthy file produces. A supplier LUT
that parses wrong (wrong size, wrong order, wrong domain) moves those numbers
by orders of magnitude — that is the signal this tool exists to surface.

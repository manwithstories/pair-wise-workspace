# LUTGrade

Offline C++17 batch color processor for handoff checks. It owns the 16-bit
PPM/PGM reader/writer, `.cube` parser, color transforms, LUT kernels, tiled
renderer, histogram statistics, and JSON reporting. There are no third-party
dependencies or external services.

## Build

```sh
cmake -S . -B build
cmake --build build -j
ctest --test-dir build --output-on-failure
```

The tests generate all fixture data locally with a fixed seed.

## Demo

```sh
./build/lutgrade gen-fixture --seed 7 --size 512x512 --out out/frames
./build/lutgrade grade \
  --lut out/frames/grade33.cube --in out/frames/log_ramp.ppm \
  --src acescg --dst rec709 --interp tetra --threads 4 \
  --out out/graded.ppm --report out/report.json
./build/lutgrade verify --lut out/frames/grade33.cube --in out/frames/log_ramp.ppm
```

`gen-fixture` creates `log_ramp.ppm`, `color_bars.ppm`, and a combined
`.cube` containing a 1024-entry 1D shaper followed by a 33x33x33 table.
The PPM files use binary P6 with `maxval=65535`; the reader accepts binary
P5/P6 with either 8-bit (`maxval < 256`) or 16-bit samples.

## Color Pipeline

For each pixel the renderer performs these operations in order:

1. Normalize the 16-bit image sample into floating point.
2. Decode the source transfer function to linear RGB.
3. Convert source RGB primaries to destination primaries, including Bradford
   chromatic adaptation when the white points differ.
4. Apply the optional 1D shaper.
5. Sample the 3D LUT in the destination gamut.
6. Encode the destination transfer function and clamp the final display code.

The supported spaces are sRGB, Rec.709, Rec.2020, DCI-P3 (DCI white), and
ACEScg. ACEScg is linear; the other destinations use their standard inverse/
forward transfer curves. Conversions between different white points use
Bradford adaptation. The input fixture is a log-distributed linear ramp, so
no display gamma is baked into its stored samples.

Trilinear and tetrahedral interpolation share the same template kernel. Both
normalize coordinates with `DOMAIN_MIN`/`DOMAIN_MAX` and clamp outside that
range to the LUT edge. A combined LUT is evaluated shaper-first, then 3D.

The scalar and row-band renderers call the same per-pixel function. Therefore
`verify` can compare them pixel by pixel; the acceptance threshold is `1e-6`.

## Reports

The report contains:

- 3D grid dimensions and 1D shaper size;
- per-channel maximum scalar/tiled deviation;
- 256-bin before/after histograms for R, G, and B;
- nonzero bucket counts and verification status;
- source, destination, interpolation, thread count, and input paths.

The console summary is emitted after the image and report have been committed.
Outputs are written to hidden temporary files in the destination directory and
atomically renamed, so a failed write never leaves a half-written final file.
No system temporary directory is used.

## Exit Codes

- `0`: success
- `1`: invalid command or parameter
- `2`: invalid `.cube` LUT
- `3`: image, allocation, or I/O failure
- `4`: scalar/tiled verification exceeded `1e-6`

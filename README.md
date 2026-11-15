# subtitle-audit

Offline ASS subtitle audit tool for delivery teams.  It parses an ASS script,
resolves inherited styles, lexes inline override tags, splits text runs, and
renders each event off-screen through libass to report visibility and pixel
bounding boxes.  No GUI, browser, device permission or network service is used
at runtime.

## Dependencies and build

Required: C++17 compiler, CMake 3.20+, libass 0.17.1+, FreeType 2.13+,
HarfBuzz 8.0+, FriBidi and Fontconfig.

```sh
./scripts/setup_deps.sh
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j4
```

`setup_deps.sh` checks the versioned system libraries, downloads libass 0.17.4
from its official release into `vendor/`, and installs a static build under
`vendor/install`.  CMake prepends that prefix and otherwise uses `pkg-config`.

## Commands

```sh
./build/subtitle-audit gen-fixtures --out fixtures --count 12 --seed 20261116
./build/subtitle-audit style-chain --script fixtures/ep12_zh.ass --json out/chain.json
./build/subtitle-audit tag-stats --script fixtures/ep12_zh.ass --top 5
./build/subtitle-audit render-probe --script fixtures/ep12_zh.ass --frame 1250 \
  --out out/probe.json --dump-alpha out/alpha
./build/subtitle-audit batch --dir fixtures --out out/batch.json
```

`style-chain` resolves the leaf-to-root chain and includes per-event character
segments with the active chain and override snapshot.  `tag-stats` prints both
aggregate and per-event counts.  `render-probe` writes one cropped grayscale PPM
per visible event plus an FNV-1a alpha digest.  `batch` visits `*.ass` files in
filename dictionary order and writes its report before returning a partial
missing-font status.

Pass `--json -` (or `--out -` for render/batch) to emit the machine-readable
report to stdout.  `ctest --test-dir build --output-on-failure` runs the two
local smoke/edge suites.

## Style inheritance

ASS has no native style parent field.  The tool accepts a comment directive in
the styles section:

```ass
;Parent: ep12_zh=ep12
;Parent: ep12=season1
;Parent: season1=template
```

It also accepts `Parent`, `Inherit` or `Base` as a style Format field.  Without
an explicit directive it infers common delivery names: language suffix to base
episode, `epNN` to `season1`, title suffix to its base style, and nested name
prefixes.  Empty values and `*` inherit from the parent; non-empty values
override it.  Missing parents and cycles produce line diagnostics and stop the
audit.

## Exit codes

| code | meaning |
| --- | --- |
| 0 | all checks passed |
| 2 | command-line argument, directory or output error |
| 3 | ASS parse failure, missing style/parent or inheritance cycle |
| 4 | libass initialization, track load, render or PPM failure |
| 5 | one or more events use an unavailable font; reports are still written |

Parse diagnostics include line numbers.  Duplicate style names are reported but
do not by themselves stop the audit.  Unknown tags and unclosed override braces
are counted and do not abort rendering.

## Source layout

- `src/ass_doc.cpp`: tolerant ASS parsing, BOM/CRLF, Format reordering, diagnostics
- `src/style_chain.cpp`: parent inference, field inheritance, cycle detection
- `src/tag_lexer.cpp`: escape-aware lexer and nested transformation parsing
- `src/tag_effect.cpp`: override evaluation and tag aggregation
- `src/run_split.cpp`: run boundaries and effective chains
- `src/layout_probe.cpp`: normalized ASS, libass off-screen rendering, font checks
- `src/digest.cpp`: alpha digest and PPM output
- `src/report.cpp`: JSON chain, tag, probe and batch reports
- `src/command_utils.cpp`, `src/batch_command.cpp`: CLI parsing and batch execution
- `src/main.cpp`: CLI and exit codes
- `tools/gen_fixtures.cpp`: deterministic fixture generator

Generated `build/`, `fixtures/`, `out/` and `vendor/` build/download artifacts
are ignored by Git.

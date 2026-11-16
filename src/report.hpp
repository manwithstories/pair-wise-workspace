#pragma once
#include <string>

#include "interpolator.hpp"
#include "statistics.hpp"

// Terminal report formatting.
//
// Kept separate from main so the exact output contract (KEY=VALUE lines the
// acceptance checks grep for) lives in one place and is unit-reachable.
namespace lutgrade {

struct RunReport {
  int grid_size = 0;
  std::size_t pixel_count = 0;
  std::string path;          // "scalar" or "tiled"
  int threads = 1;
  std::string in_space;
  std::string out_space;
  Statistics stats;
};

// Prints the full report to stdout. Exactly one line per key, stable order:
//   GRID_SIZE, PIXELS, PATH, THREADS, IN_SPACE, OUT_SPACE,
//   STAGE decode=<x> matrix=<y> encode=<z> lut=<n>,
//   MAX_DEV_R/G/B, MEAN_DEV, HIST_BEFORE_R/G/B, HIST_AFTER_R/G/B,
//   SATURATED_PIXELS, HASH
void print_run_report(const RunReport& report);

// Prints the conversion stage log (one line) describing what was applied.
void print_stage_log(const GradeSettings& settings);

// compare output:
//   COMPARE_RESULT=MATCH|MISMATCH, PIXELS, THRESHOLD, MAX_DIFF,
//   FIRST_DIFF=x,y,channel=name,left=..,right=..
struct CompareReport {
  bool match = true;
  std::size_t pixels = 0;
  double threshold = 0.0;
  double max_diff = 0.0;
  bool has_first_diff = false;
  std::size_t first_x = 0;
  std::size_t first_y = 0;
  int first_channel = 0;
  float left = 0.0f;
  float right = 0.0f;
  std::uint64_t hash_scalar = 0;
  std::uint64_t hash_tiled = 0;
};

void print_compare_report(const CompareReport& report);

}  // namespace lutgrade

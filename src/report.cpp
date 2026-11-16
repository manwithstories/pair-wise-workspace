#include "report.hpp"

#include <cstdio>

namespace lutgrade {
namespace {

const char* kChannelNames[3] = {"R", "G", "B"};

void print_histogram(const char* key, const std::array<std::uint64_t, 256>& hist) {
  std::printf("%s=", key);
  for (std::size_t i = 0; i < hist.size(); ++i) {
    if (i) std::printf(",");
    std::printf("%llu", static_cast<unsigned long long>(hist[i]));
  }
  std::printf("\n");
}

}  // namespace

void print_stage_log(const GradeSettings& settings) {
  std::printf("STAGE=decode:%s matrix:%s->%s encode:%s lut:trilinear\n",
              transfer_name(settings.in_space), color_space_name(settings.in_space),
              color_space_name(settings.out_space), transfer_name(settings.out_space));
}

void print_run_report(const RunReport& report) {
  std::printf("GRID_SIZE=%d\n", report.grid_size);
  std::printf("PIXELS=%zu\n", report.pixel_count);
  std::printf("PATH=%s\n", report.path.c_str());
  std::printf("THREADS=%d\n", report.threads);
  std::printf("IN_SPACE=%s\n", report.in_space.c_str());
  std::printf("OUT_SPACE=%s\n", report.out_space.c_str());

  std::printf("MAX_DEV_R=%.9f\n", report.stats.deviation[0].max_abs);
  std::printf("MAX_DEV_G=%.9f\n", report.stats.deviation[1].max_abs);
  std::printf("MAX_DEV_B=%.9f\n", report.stats.deviation[2].max_abs);
  std::printf("MEAN_DEV=%.9f\n", report.stats.mean_deviation);

  print_histogram("HIST_BEFORE_R", report.stats.hist_before[0]);
  print_histogram("HIST_BEFORE_G", report.stats.hist_before[1]);
  print_histogram("HIST_BEFORE_B", report.stats.hist_before[2]);
  print_histogram("HIST_AFTER_R", report.stats.hist_after[0]);
  print_histogram("HIST_AFTER_G", report.stats.hist_after[1]);
  print_histogram("HIST_AFTER_B", report.stats.hist_after[2]);

  std::printf("SATURATED_PIXELS=%llu\n",
              static_cast<unsigned long long>(report.stats.saturated_pixels));
  std::printf("HASH=%s\n", format_hash(report.stats.hash).c_str());
}

void print_compare_report(const CompareReport& report) {
  std::printf("COMPARE_RESULT=%s\n", report.match ? "MATCH" : "MISMATCH");
  std::printf("PIXELS=%zu\n", report.pixels);
  std::printf("THRESHOLD=%.9f\n", report.threshold);
  std::printf("MAX_DIFF=%.9f\n", report.max_diff);
  if (report.has_first_diff) {
    const char* channel = (report.first_channel >= 0 && report.first_channel < 3)
                              ? kChannelNames[report.first_channel]
                              : "?";
    std::printf("FIRST_DIFF_X=%zu\n", report.first_x);
    std::printf("FIRST_DIFF_Y=%zu\n", report.first_y);
    std::printf("FIRST_DIFF_CHANNEL=%s\n", channel);
    std::printf("FIRST_DIFF_LEFT=%.9f\n", static_cast<double>(report.left));
    std::printf("FIRST_DIFF_RIGHT=%.9f\n", static_cast<double>(report.right));
  }
  std::printf("HASH_SCALAR=%s\n", format_hash(report.hash_scalar).c_str());
  std::printf("HASH_TILED=%s\n", format_hash(report.hash_tiled).c_str());
}

}  // namespace lutgrade

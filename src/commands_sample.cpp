#include <cinttypes>
#include <cstdio>
#include <string>
#include <utility>
#include <vector>

#include "cli_options.hpp"
#include "commands.hpp"
#include "json_report.hpp"
#include "media_types.hpp"
#include "pipeline.hpp"
#include "sample_generator.hpp"

namespace mfs {
namespace {

void print_json_summary(const RepairResult& result, const std::string& path) {
  std::printf("{\n");
  std::printf("  \"roundtrip_match_rate\": %.6f,\n", result.roundtrip_match_rate);
  std::printf("  \"keyframe_index_hit_rate\": %.6f,\n",
              result.keyframe_index_hit_rate);
  std::printf("  \"segment_hash_match_rate\": %.6f,\n",
              result.segment_hash_match_rate);
  std::printf("  \"repaired_frames\": %" PRIu64 ",\n", result.repaired_frames);
  std::printf("  \"duplicate_packets\": %" PRIu64 ",\n", result.duplicate_packets);
  std::printf("  \"lost_ranges\": %" PRIu64 ",\n", result.lost_ranges);
  std::printf("  \"decode_errors\": %" PRIu64 ",\n", result.decode_errors);
  std::printf("  \"first_error_offset\": %" PRId64 ",\n",
              result.first_error_offset);
  std::printf("  \"report\": \"%s\"\n", path.c_str());
  std::printf("}\n");
}

}  // namespace

int cmd_generate(const OptionTable& options) {
  GeneratorOptions generator;
  uint64_t value = 0;
  double rate = 0.0;
  std::string out_path;
  if (!options.get_u32("seed", generator.seed) ||
      !options.get_u32("frames", generator.frames) ||
      !options.get_u32("loss-burst", generator.loss_burst) ||
      !options.get_u32("reorder-window", generator.reorder_window) ||
      !options.get("out", out_path) ||
      !options.get_double("duplicate-rate", rate)) {
    std::fprintf(stderr,
                 "generate requires --seed --frames --loss-burst "
                 "--duplicate-rate --reorder-window --out\n");
    return kExitUsageOrFormat;
  }
  generator.duplicate_rate = rate;
  if (rate < 0.0 || rate > 1.0) {
    return kExitUsageOrFormat;
  }
  if (options.has("width") &&
      !options.get_u32("width", generator.width)) {
    return kExitUsageOrFormat;
  }
  if (options.has("height") &&
      !options.get_u32("height", generator.height)) {
    return kExitUsageOrFormat;
  }
  if (options.has("keyframe-interval") &&
      !options.get_u32("keyframe-interval", generator.keyframe_interval)) {
    return kExitUsageOrFormat;
  }
  if (options.has("loss-events") &&
      !options.get_u32("loss-events", generator.loss_events)) {
    return kExitUsageOrFormat;
  }
  if (options.has("timestamp-defects") &&
      !options.get_u32("timestamp-defects", generator.timestamp_defects)) {
    return kExitUsageOrFormat;
  }
  if (options.has("frame-rate") &&
      !options.get_u32("frame-rate", generator.frame_rate)) {
    return kExitUsageOrFormat;
  }
  for (const std::string& gap_text : options.get_all("arrival-gap")) {
    if (!parse_u64(gap_text, value) || value > UINT32_MAX) {
      return kExitUsageOrFormat;
    }
    generator.arrival_gaps.push_back(static_cast<uint32_t>(value));
  }
  if (generator.frames == 0 || generator.frame_rate == 0 ||
      generator.width == 0 || generator.height == 0 ||
      generator.reorder_window == 0 || generator.keyframe_interval == 0) {
    return kExitUsageOrFormat;
  }

  std::vector<Packet> packets;
  const GenerationStats stats = generate_sample(generator, packets);
  try {
    write_container(out_path, packets, generator.seed);
  } catch (const MediaError& error) {
    std::fprintf(stderr, "generate failed: %s\n", error.what());
    return kExitInternal;
  }
  std::printf("{\n");
  std::printf("  \"out\": \"%s\",\n", out_path.c_str());
  std::printf("  \"seed\": %u,\n", generator.seed);
  std::printf("  \"frames\": %u,\n", stats.frames);
  std::printf("  \"packets\": %u,\n", stats.packets);
  std::printf("  \"duplicates\": %u,\n", stats.duplicates);
  std::printf("  \"reordered\": %u,\n", stats.reordered);
  std::printf("  \"dropped\": %u,\n", stats.dropped);
  std::printf("  \"clock_defects\": %u\n", stats.clock_defects);
  std::printf("}\n");
  return kExitOk;
}

int cmd_repair(const OptionTable& options) {
  std::string input;
  std::string json_path;
  PipelineOptions pipeline;
  uint64_t recovered = 0;
  if (!options.get("input", input) || !options.get("json", json_path) ||
      !options.get_u32("segment-frames", pipeline.segment_frames)) {
    std::fprintf(stderr,
                 "repair requires --input --segment-frames --json\n");
    return kExitUsageOrFormat;
  }
  if (options.has("max-segments") &&
      !options.get_u32("max-segments", pipeline.max_segments)) {
    return kExitUsageOrFormat;
  }
  if (options.has("max-frame-bytes") &&
      !options.get_u64("max-frame-bytes", recovered)) {
    return kExitUsageOrFormat;
  }
  if (recovered > 0) {
    pipeline.parse.max_frame_bytes = static_cast<size_t>(recovered);
  }
  recovered = 0;
  if (options.has("max-total-bytes") &&
      !options.get_u64("max-total-bytes", recovered)) {
    return kExitUsageOrFormat;
  }
  if (recovered > 0) {
    pipeline.parse.max_total_bytes = static_cast<size_t>(recovered);
  }
  if (options.has("reorder-window") &&
      !options.get_u32("reorder-window", pipeline.reorder.base_window)) {
    return kExitUsageOrFormat;
  }

  std::vector<uint8_t> data;
  try {
    data = read_file(input);
  } catch (const MediaError& error) {
    std::fprintf(stderr, "%s\n", error.what());
    return kExitUsageOrFormat;
  }

  const RepairResult result = run_pipeline(data, pipeline);
  const bool unrecoverable =
      result.empty_sequence && result.input_bytes < kFileHeaderSize;
  if (unrecoverable) {
    std::fprintf(stderr,
                 "input is shorter than the %zu-byte file header\n",
                 kFileHeaderSize);
    return kExitUsageOrFormat;
  }
  const std::string report = report_json(result, pipeline);
  if (!write_text_file(json_path, report)) {
    std::fprintf(stderr, "cannot write JSON report: %s\n", json_path.c_str());
    return kExitUsageOrFormat;
  }
  print_json_summary(result, json_path);
  return kExitOk;
}

}  // namespace mfs

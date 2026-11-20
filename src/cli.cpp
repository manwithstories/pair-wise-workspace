#include "media/cli.hpp"

#include "media/frame_reader.hpp"
#include "media/generate.hpp"
#include "media/sha256.hpp"

#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <filesystem>
#include <map>
#include <string>
#include <string_view>
#include <vector>

namespace mediaqc {
namespace {

namespace fs = std::filesystem;

// Minimal flag parser: `--name value` and `--flag`.  Unknown flags are errors
// so a typo never silently changes behaviour.
class Args {
 public:
  Args(int argc, char** argv, int start) {
    for (int i = start; i < argc; ++i) {
      tokens_.emplace_back(argv[i]);
    }
  }
  explicit Args(const std::vector<std::string>& tokens) : tokens_(tokens) {}

  bool has(std::string_view name) const {
    for (const auto& token : tokens_) {
      if (token == name) return true;
    }
    return false;
  }

  std::string value(std::string_view name, const std::string& fallback = "") const {
    for (std::size_t i = 0; i + 1 < tokens_.size(); ++i) {
      if (tokens_[i] == name) return tokens_[i + 1];
    }
    return fallback;
  }

  std::uint64_t u64(std::string_view name, std::uint64_t fallback) const {
    std::string text = value(name);
    if (text.empty()) return fallback;
    char* end = nullptr;
    unsigned long long parsed = std::strtoull(text.c_str(), &end, 10);
    if (!end || *end != '\0') return fallback;
    return static_cast<std::uint64_t>(parsed);
  }

  // Reports the first token that is not a known flag or its value.
  std::string validate(const std::vector<std::string>& valued,
                       const std::vector<std::string>& flags) const {
    for (std::size_t i = 0; i < tokens_.size(); ++i) {
      const std::string& token = tokens_[i];
      if (token.empty() || token[0] != '-') {
        return "unexpected argument '" + token + "'";
      }
      bool is_valued = false;
      for (const auto& name : valued) is_valued = is_valued || token == name;
      bool is_flag = false;
      for (const auto& name : flags) is_flag = is_flag || token == name;
      if (is_valued) {
        if (i + 1 >= tokens_.size()) return "missing value for " + token;
        ++i;
      } else if (!is_flag) {
        return "unknown option '" + token + "'";
      }
    }
    return {};
  }

 private:
  std::vector<std::string> tokens_;
};

int fail(int code, const std::string& message) {
  std::fprintf(stderr, "mediaqc: %s\n", message.c_str());
  return code;
}

int fail(const Error& error) {
  std::fprintf(stderr, "mediaqc: %s\n", error.message.c_str());
  return error.exit_code();
}

}  // namespace

void print_usage() {
  std::printf(
      "mediaqc - local broadcast material repair, re-mux and verification\n"
      "\n"
      "usage:\n"
      "  mediaqc generate --scenario NAME --out DIR [--frames N] [--seed N]\n"
      "  mediaqc process  --input DIR --output DIR [--queue-frames N]\n"
      "                   [--queue-bytes N] [--segment-frames N]\n"
      "                   [--checkpoint FILE] [--force] [--stop-after N]\n"
      "  mediaqc verify   --input DIR [--json]\n"
      "  mediaqc query    --input DIR [--track ID] [--time-ns N | --frame N]\n"
      "\n"
      "exit codes: 0 ok, 2 invalid arguments/input, 3 damage found, "
      "4 I/O or recovery failure\n");
}

int run_generate(const std::vector<std::string>& args) {
  Args parsed(args);
  std::string problem = parsed.validate(
      {"--scenario", "--out", "--frames", "--seed", "--queue-frames"}, {});
  if (!problem.empty()) return fail(kExitUsage, problem);
  const std::string scenario_name = parsed.value("--scenario");
  const std::string out = parsed.value("--out");
  if (scenario_name.empty() || out.empty()) {
    return fail(kExitUsage, "generate requires --scenario and --out");
  }
  Scenario scenario;
  if (!parse_scenario(scenario_name, scenario)) {
    return fail(kExitUsage, "unknown scenario '" + scenario_name + "'");
  }
  GenerateOptions options;
  options.scenario = scenario;
  options.frames = parsed.u64("--frames", options.frames);
  options.seed = parsed.u64("--seed", options.seed);
  if (options.frames == 0) {
    return fail(kExitUsage, "--frames must be positive");
  }
  auto result = generate_delivery(out, options, false);
  if (!result.ok) {
    return fail(result.error);
  }
  std::printf("scenario=%s frames=%llu tracks=%llu first_corrupt_offset=%lld\n",
              std::string(to_string(scenario)).c_str(),
              static_cast<unsigned long long>(result.frames),
              static_cast<unsigned long long>(result.tracks),
              static_cast<long long>(result.first_corrupt_offset));
  return kExitOk;
}

int run_process(const std::vector<std::string>& args) {
  Args parsed(args);
  std::string problem = parsed.validate(
      {"--input", "--output", "--queue-frames", "--queue-bytes",
       "--segment-frames", "--checkpoint", "--stop-after"},
      {"--force", "--json"});
  if (!problem.empty()) return fail(kExitUsage, problem);

  PipelineOptions options;
  options.input_dir = parsed.value("--input");
  options.output_dir = parsed.value("--output");
  if (options.input_dir.empty() || options.output_dir.empty()) {
    return fail(kExitUsage, "process requires --input and --output");
  }
  options.queue_frames = parsed.u64("--queue-frames", options.queue_frames);
  options.queue_bytes = parsed.u64("--queue-bytes", options.queue_bytes);
  options.segment_frames = parsed.u64("--segment-frames", options.segment_frames);
  options.checkpoint_path = parsed.value("--checkpoint");
  options.force = parsed.has("--force");
  options.stop_after_frames = parsed.u64("--stop-after", 0);
  if (options.queue_frames == 0) {
    return fail(kExitUsage, "--queue-frames must be positive");
  }
  if (options.segment_frames == 0) {
    return fail(kExitUsage, "--segment-frames must be positive");
  }

  Pipeline pipeline(options);
  PipelineReport report = pipeline.run();
  if (!report.ok) {
    return fail(report.error);
  }
  const bool json = parsed.has("--json");
  if (json) {
    Json summary = report.to_json();
    std::printf("%s\n", dump_json(summary, true).c_str());
  } else {
    std::printf("frames_total=%llu\n",
                static_cast<unsigned long long>(report.frames_total));
    std::printf("frames_repaired=%llu\n",
                static_cast<unsigned long long>(report.frames_repaired));
    std::printf("first_corrupt_offset=%lld\n",
                static_cast<long long>(report.first_corrupt_offset));
    std::printf("peak_queue_frames=%llu\n",
                static_cast<unsigned long long>(report.peak_queue_frames));
    std::printf("resume_ms=%lld\n", static_cast<long long>(report.resume_ms));
    std::printf("output_sha256=%s\n", report.output_sha256.c_str());
    std::printf("index_entries=%llu\n",
                static_cast<unsigned long long>(report.index_entries));
  }
  if (report.interrupted) return kExitOk;
  if (report.frames_corrupt > 0 || report.frames_truncated > 0) {
    return kExitMedia;
  }
  return kExitOk;
}

int run_verify(const std::vector<std::string>& args) {
  Args parsed(args);
  std::string problem = parsed.validate({"--input"}, {"--json"});
  if (!problem.empty()) return fail(kExitUsage, problem);
  const std::string input = parsed.value("--input");
  if (input.empty()) return fail(kExitUsage, "verify requires --input");

  MediaIndex index;
  Error error;
  if (!load_index(input, index, error)) {
    return fail(error);
  }
  Manifest manifest;
  if (!load_manifest((fs::path(input) / "manifest.json").string(), manifest, error)) {
    return fail(error);
  }

  // Re-check every segment digest and every index entry against the files.
  std::uint64_t segment_mismatch = 0;
  for (const SegmentInfo& segment : index.segments) {
    const std::string path = (fs::path(input) / segment.path).string();
    std::string hash_error;
    Sha256Digest digest = sha256_of_file(path, hash_error);
    if (!hash_error.empty() || to_hex(digest) != segment.sha256) {
      ++segment_mismatch;
    }
  }
  std::uint64_t out_of_bounds = 0;
  for (const IndexEntry& entry : index.entries) {
    if (entry.segment >= index.segments.size()) {
      ++out_of_bounds;
      continue;
    }
    const std::string path =
        (fs::path(input) / index.segments[entry.segment].path).string();
    std::error_code ec;
    std::uintmax_t size = fs::file_size(path, ec);
    if (ec || entry.payload_offset + entry.payload_size > size) {
      ++out_of_bounds;
    }
  }

  // The manifest's combined output digest must still match the segments.
  Sha256 hasher;
  std::vector<SegmentInfo> ordered = index.segments;
  std::sort(ordered.begin(), ordered.end(),
            [](const SegmentInfo& a, const SegmentInfo& b) { return a.path < b.path; });
  for (const SegmentInfo& segment : ordered) {
    hasher.update(segment.sha256);
    hasher.update("\n");
  }
  const std::string combined = to_hex(hasher.finish());
  const bool digest_ok = combined == manifest.output_sha256;

  const bool ok = segment_mismatch == 0 && out_of_bounds == 0 && digest_ok;
  if (parsed.has("--json")) {
    Json summary = Json::make_object();
    summary.set("frames_total", Json::make_int(static_cast<std::int64_t>(manifest.frames_total)));
    summary.set("index_entries", Json::make_int(static_cast<std::int64_t>(index.entries.size())));
    summary.set("segments", Json::make_int(static_cast<std::int64_t>(index.segments.size())));
    summary.set("segment_mismatch", Json::make_int(static_cast<std::int64_t>(segment_mismatch)));
    summary.set("out_of_bounds", Json::make_int(static_cast<std::int64_t>(out_of_bounds)));
    summary.set("output_sha256", Json::make_string(manifest.output_sha256));
    summary.set("recomputed_sha256", Json::make_string(combined));
    summary.set("digest_match", Json::make_bool(digest_ok));
    summary.set("ok", Json::make_bool(ok));
    std::printf("%s\n", dump_json(summary, true).c_str());
  } else {
    std::printf("index_entries=%llu segments=%llu segment_mismatch=%llu "
                "out_of_bounds=%llu digest_match=%d output_sha256=%s\n",
                static_cast<unsigned long long>(index.entries.size()),
                static_cast<unsigned long long>(index.segments.size()),
                static_cast<unsigned long long>(segment_mismatch),
                static_cast<unsigned long long>(out_of_bounds),
                digest_ok ? 1 : 0, manifest.output_sha256.c_str());
  }
  return ok ? kExitOk : kExitMedia;
}

int run_query(const std::vector<std::string>& args) {
  Args parsed(args);
  std::string problem = parsed.validate({"--input", "--track", "--time-ns", "--frame"}, {});
  if (!problem.empty()) return fail(kExitUsage, problem);
  const std::string input = parsed.value("--input");
  if (input.empty()) return fail(kExitUsage, "query requires --input");

  MediaIndex index;
  Error error;
  if (!load_index(input, index, error)) {
    return fail(error);
  }
  std::string track = parsed.value("--track");
  if (track.empty() && !index.tracks.empty()) track = index.tracks.front().id;

  const IndexEntry* entry = nullptr;
  if (parsed.has("--time-ns")) {
    Ns time = static_cast<Ns>(static_cast<std::int64_t>(
        std::strtoll(parsed.value("--time-ns").c_str(), nullptr, 10)));
    entry = find_by_time(index, track, time);
  } else if (parsed.has("--frame")) {
    entry = find_by_frame(index, track, parsed.u64("--frame", 0));
  } else {
    return fail(kExitUsage, "query requires --time-ns or --frame");
  }
  if (!entry) {
    return fail(kExitMedia, "no index entry for track '" + track + "'");
  }
  std::printf("track=%s frame=%llu frame_index=%llu time_ns=%lld segment=%zu "
              "offset=%llu size=%llu keyframe=%d sha256=%s\n",
              entry->track.c_str(),
              static_cast<unsigned long long>(entry->frame),
              static_cast<unsigned long long>(entry->frame_index),
              static_cast<long long>(entry->time_ns), entry->segment,
              static_cast<unsigned long long>(entry->payload_offset),
              static_cast<unsigned long long>(entry->payload_size),
              entry->keyframe ? 1 : 0, to_hex(entry->sha256).c_str());
  return kExitOk;
}

}  // namespace mediaqc

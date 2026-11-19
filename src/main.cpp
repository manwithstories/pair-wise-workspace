#include <cmath>
#include <cstdio>
#include <filesystem>
#include <iostream>
#include <optional>
#include <map>
#include <cstdlib>
#include <string>
#include <string_view>
#include <vector>

#include "audio_restore.hpp"
#include "bitstream.hpp"
#include "color_metadata.hpp"
#include "error.hpp"
#include "file_io.hpp"
#include "fixture.hpp"
#include "json.hpp"
#include "manifest.hpp"
#include "restore_engine.hpp"
#include "timeline.hpp"

namespace {

using namespace media_restore;

void usage(std::ostream& out) {
  out << "usage: media-restore <command> [options]\n"
         "\n"
         "commands:\n"
         "  generate --out DIR [--include-damaged]\n"
         "      Write a deterministic sample archive to DIR.\n"
         "  inspect --manifest FILE [--json FILE]\n"
         "      Validate a manifest, its keyframe indexes and track graph.\n"
         "  restore --manifest FILE --rate RATE --layout LAYOUT --loudness LUFS\n"
         "          [--true-peak DBTP] [--color preserve|archive]\n"
         "          [--archive-primaries V] [--archive-transfer V] [--archive-range V]\n"
         "          [--out DIR] [--json FILE]\n"
         "      Validate media, order tracks, restore audio and hash outputs.\n"
         "  verify --manifest FILE --result FILE [--json FILE]\n"
         "      Re-check a restore result against the archive on disk.\n"
         "\n"
         "exit codes: 0 verified, 1 usage/input error, 2 media validation\n"
         "            failure, 3 restore or dependency failure\n";
}

struct Args {
  std::string command;
  std::vector<std::pair<std::string, std::string>> options;
  std::vector<std::string> flags;

  bool has(const std::string& name) const {
    for (const auto& flag : flags) {
      if (flag == name) return true;
    }
    return false;
  }

  bool get(const std::string& name, std::string* value) const {
    for (const auto& option : options) {
      if (option.first == name) {
        *value = option.second;
        return true;
      }
    }
    return false;
  }

  bool get_int(const std::string& name, int64_t* value) const {
    std::string text;
    if (!get(name, &text)) return false;
    if (text.empty()) return false;
    char* end = nullptr;
    const long long parsed = std::strtoll(text.c_str(), &end, 10);
    if (end != text.c_str() + text.size()) return false;
    *value = parsed;
    return true;
  }

  bool has_option(const std::string& name) const {
    for (const auto& option : options) {
      if (option.first == name) return true;
    }
    return false;
  }

  bool get_double(const std::string& name, double* value) const {
    std::string text;
    if (!get(name, &text)) return false;
    if (text.empty()) return false;
    char* end = nullptr;
    const double parsed = std::strtod(text.c_str(), &end);
    if (end != text.c_str() + text.size() || !std::isfinite(parsed)) return false;
    *value = parsed;
    return true;
  }
};

bool parse_args(int argc, char** argv, Args* args, Diagnostic* error) {
  if (argc < 2) {
    error->code = ErrorCode::usage;
    error->message = "missing command";
    return false;
  }
  args->command = argv[1];
  for (int i = 2; i < argc; ++i) {
    std::string_view arg = argv[i];
    if (arg == "--help" || arg == "-h") {
      args->flags.emplace_back("--help");
      continue;
    }
    // Value-less switches; a manifest cannot express these as options.
    if (arg == "--include-damaged") {
      args->flags.emplace_back(arg);
      continue;
    }
    if (arg.rfind("--", 0) != 0) {
      error->code = ErrorCode::usage;
      error->message = "unexpected positional argument: " + std::string(arg);
      return false;
    }
    std::string name(arg);
    const auto eq = name.find('=');
    if (eq != std::string::npos) {
      args->options.emplace_back(name.substr(0, eq), name.substr(eq + 1));
      continue;
    }
    if (i + 1 >= argc) {
      error->code = ErrorCode::usage;
      error->message = "option requires a value: " + name;
      return false;
    }
    args->options.emplace_back(std::move(name), argv[++i]);
  }
  return true;
}

int fail(const Diagnostic& diagnostic) {
  print_diagnostics({diagnostic});
  return exit_code_for(diagnostic.code);
}

int fail(std::vector<Diagnostic> diagnostics) {
  print_diagnostics(diagnostics);
  return exit_code_for(diagnostics);
}

bool write_json_if_requested(const Args& args, const std::string& name,
                             const Json& document, Diagnostic* error) {
  std::string path;
  if (!args.get(name, &path)) return true;
  return write_file_atomic(path, document.dump(), error);
}

Diagnostic usage_error(const std::string& message) {
  Diagnostic diagnostic;
  diagnostic.code = ErrorCode::usage;
  diagnostic.message = message;
  return diagnostic;
}

// ---------------------------------------------------------------------------
// generate
// ---------------------------------------------------------------------------

int command_generate(const Args& args) {
  std::string out;
  if (!args.get("--out", &out) || out.empty()) {
    return fail(usage_error("generate requires --out DIR"));
  }
  FixtureOptions options;
  options.out_dir = out;
  options.include_damaged = args.has("--include-damaged");
  std::vector<Diagnostic> diagnostics;
  if (!generate_archive(options, &diagnostics)) {
    if (diagnostics.empty()) {
      Diagnostic diagnostic;
      diagnostic.code = ErrorCode::internal;
      diagnostic.message = "fixture generation failed";
      diagnostics.push_back(std::move(diagnostic));
    }
    return fail(std::move(diagnostics));
  }
  print_diagnostics(diagnostics);
  std::cout << "archive=" << (std::filesystem::path(out) / "archive.json").string()
            << '\n';
  std::cout << "damaged=" << (options.include_damaged ? "included" : "excluded")
            << '\n';
  return 0;
}

// ---------------------------------------------------------------------------
// inspect
// ---------------------------------------------------------------------------

struct InspectReport {
  size_t tracks = 0;
  size_t segments = 0;
  size_t keyframe_hits = 0;        // healthy segments only
  size_t keyframe_total = 0;       // healthy segments only
  size_t keyframe_generated = 0;
  size_t quarantined = 0;
  size_t quarantined_hits = 0;     // reported separately, never in the ratio
  size_t quarantined_total = 0;
  size_t problems = 0;
  Timeline timeline;
  std::vector<Diagnostic> diagnostics;
};

// Validates every segment's bitstream and index without writing media.
// Structural diagnostics are attributed to the segment they name, so a
// quarantined segment can confirm its declared defect instead of failing the
// whole inspection.
bool inspect_archive(const Manifest& manifest, const Limits& limits,
                     InspectReport* report) {
  Timeline timeline;
  std::vector<Diagnostic> structural;
  build_timeline(manifest, &structural, &timeline);
  report->timeline = std::move(timeline);
  std::map<std::pair<std::string, std::string>, std::vector<Diagnostic>>
      by_segment;
  for (auto& diagnostic : structural) {
    if (!diagnostic.location.segment.empty()) {
      by_segment[{diagnostic.location.track, diagnostic.location.segment}]
          .push_back(std::move(diagnostic));
    } else {
      report->diagnostics.push_back(std::move(diagnostic));
    }
  }

  std::map<std::string, StreamScan> cache;
  for (const auto& track : manifest.tracks) {
    ++report->tracks;
    for (const auto& segment : track.segments) {
      ++report->segments;
      if (segment.expected_code) ++report->quarantined;
      std::vector<Diagnostic> segment_diagnostics;
      auto held = by_segment.find({track.id, segment.id});
      if (held != by_segment.end()) segment_diagnostics = std::move(held->second);
      const bool audio_only =
          track.kind == TrackKind::audio && segment.audio.has_value();

      std::filesystem::path media_path;
      Diagnostic error;
      bool segment_ok = true;
      if (!resolve_archive_path(manifest.source_path, segment.media,
                                &media_path, &error)) {
        segment_diagnostics.push_back(std::move(error));
        segment_ok = false;
      }
      if (segment_ok && !audio_only) {
        std::string prefix;
        if (!read_prefix(media_path, 0, 64, &prefix, &error)) {
          segment_diagnostics.push_back(std::move(error));
          segment_ok = false;
        } else {
          Codec sniffed = segment.codec;
          if (!sniff_codec(prefix, &sniffed) || sniffed != segment.codec) {
            Location where;
            where.track = track.id;
            where.segment = segment.id;
            where.file = media_path.string();
            where.byte_offset = 0;
            Diagnostic diagnostic;
            diagnostic.code = ErrorCode::unknown_codec;
            diagnostic.message = "content does not match the declared codec";
            diagnostic.location = where;
            segment_diagnostics.push_back(std::move(diagnostic));
            segment_ok = false;
          }
        }
      }
      if (segment_ok) {
        const std::string key =
            media_path.string() + "/" + std::to_string(static_cast<int>(segment.codec));
        auto found = cache.find(key);
        if (found == cache.end()) {
          StreamScan scan;
          std::vector<Diagnostic> scan_diagnostics;
          if (!scan_raw_stream(media_path, segment.codec, limits, &scan,
                               &scan_diagnostics)) {
            for (auto& diagnostic : scan_diagnostics) {
              diagnostic.location.track = track.id;
              diagnostic.location.segment = segment.id;
              segment_diagnostics.push_back(std::move(diagnostic));
            }
            segment_ok = false;
          } else {
            found = cache.emplace(key, std::move(scan)).first;
          }
        }
        if (segment_ok) {
          KeyframeIndex index = segment.index;
          if (index.present && index.entries.empty() && !index.path.empty()) {
            std::filesystem::path index_path;
            if (!resolve_archive_path(manifest.source_path, index.path,
                                      &index_path, &error)) {
              segment_diagnostics.push_back(std::move(error));
              segment_ok = false;
            } else {
              KeyframeIndex loaded;
              std::vector<Diagnostic> index_diagnostics;
              if (!load_keyframe_index(index_path, limits, &loaded,
                                       &index_diagnostics)) {
                for (auto& diagnostic : index_diagnostics) {
                  diagnostic.location.track = track.id;
                  diagnostic.location.segment = segment.id;
                  segment_diagnostics.push_back(std::move(diagnostic));
                }
                segment_ok = false;
              } else if (index.version != loaded.version &&
                         index.version != 0) {
                // The manifest is authoritative about which index format the
                // archive was built with; a file written under another version
                // is a disagreement, not a fallback opportunity.
                Location where;
                where.track = track.id;
                where.segment = segment.id;
                where.field = "version";
                Diagnostic diagnostic;
                diagnostic.code = ErrorCode::index_version;
                diagnostic.message =
                    "index version in the manifest (" +
                    std::to_string(index.version) +
                    ") differs from the index file (" +
                    std::to_string(loaded.version) + ")";
                diagnostic.location = where;
                segment_diagnostics.push_back(std::move(diagnostic));
                segment_ok = false;
              } else {
                index.entries = std::move(loaded.entries);
                if (index.version == 0) index.version = loaded.version;
              }
            }
          }
          if (segment_ok) {
            IndexReport index_report;
            std::vector<Diagnostic> index_diagnostics;
            if (!validate_keyframe_index(index, found->second, limits,
                                         &index_report, &index_diagnostics)) {
              for (auto& diagnostic : index_diagnostics) {
                diagnostic.location.track = track.id;
                diagnostic.location.segment = segment.id;
                segment_diagnostics.push_back(std::move(diagnostic));
              }
              segment_ok = false;
            }
            if (segment.expected_code) {
              report->quarantined_hits += index_report.hits;
              report->quarantined_total += index_report.index_entries;
            } else {
              report->keyframe_hits += index_report.hits;
              report->keyframe_total += index_report.index_entries;
            }
            report->keyframe_generated += index_report.generated;
          }
        }
      }

      // Decide whether this segment is a problem.  A quarantined segment is
      // fine when one of its diagnostics carries the declared error code;
      // anything else is a real problem and its diagnostics are reported.
      if (segment.expected_code) {
        // The declared defect may be structural (an overlap) or per-segment
        // (an index defect), so the expectation is satisfied by any matching
        // error diagnostic regardless of which check produced it.
        bool matched = false;
        for (const auto& diagnostic : segment_diagnostics) {
          matched |= diagnostic.severity == Severity::error &&
                     diagnostic.code == *segment.expected_code;
        }
        if (!matched) ++report->problems;
      } else if (!segment_ok) {
        ++report->problems;
      }
      for (auto& diagnostic : segment_diagnostics) {
        if (diagnostic.severity == Severity::error) {
          report->diagnostics.push_back(std::move(diagnostic));
        }
      }
    }
  }
  return report->problems == 0;
}

int command_inspect(const Args& args) {
  std::string manifest_path;
  if (!args.get("--manifest", &manifest_path) || manifest_path.empty()) {
    return fail(usage_error("inspect requires --manifest FILE"));
  }
  const Limits limits;
  Manifest manifest;
  std::vector<Diagnostic> diagnostics;
  if (!load_manifest(manifest_path, limits, &manifest, &diagnostics)) {
    return fail(std::move(diagnostics));
  }

  InspectReport report;
  inspect_archive(manifest, limits, &report);

  // Round trip the manifest on disk to prove the schema is lossless.
  const size_t roundtripped = manifest_roundtrip_score(manifest_path, manifest);
  const double percent =
      manifest.tracks.empty()
          ? 0.0
          : 100.0 * static_cast<double>(roundtripped) /
                static_cast<double>(manifest.tracks.size());

  Json root = Json::object();
  root.set("format", Json("media-archive-inspection"));
  root.set("version", Json(int64_t{1}));
  Json time_base = Json::object();
  time_base.set("num", Json(std::to_string(manifest.time_base.num)));
  time_base.set("den", Json(std::to_string(manifest.time_base.den)));
  root.set("time_base", std::move(time_base));
  Json order = Json::array();
  for (const auto& id : report.timeline.order.order) order.push(Json(id));
  root.set("track_order", std::move(order));
  Json cyclic = Json::array();
  for (const auto& id : report.timeline.order.cyclic) cyclic.push(Json(id));
  root.set("cyclic_tracks", std::move(cyclic));
  root.set("tracks", Json(static_cast<int64_t>(report.tracks)));
  root.set("segments", Json(static_cast<int64_t>(report.segments)));
  root.set("keyframe_hits", Json(static_cast<int64_t>(report.keyframe_hits)));
  root.set("keyframe_total", Json(static_cast<int64_t>(report.keyframe_total)));
  root.set("keyframe_generated",
           Json(static_cast<int64_t>(report.keyframe_generated)));
  root.set("quarantined_keyframe_hits",
           Json(static_cast<int64_t>(report.quarantined_hits)));
  root.set("quarantined_keyframe_total",
           Json(static_cast<int64_t>(report.quarantined_total)));
  root.set("quarantined", Json(static_cast<int64_t>(report.quarantined)));
  root.set("manifest_roundtrip_percent", Json(percent));
  Json errors = Json::array();
  for (const auto& diagnostic : report.diagnostics) {
    if (!diagnostic.is_error()) continue;
    Json item = Json::object();
    item.set("code", Json(error_code_name(diagnostic.code)));
    item.set("track", Json(diagnostic.location.track));
    item.set("segment", Json(diagnostic.location.segment));
    item.set("file", Json(diagnostic.location.file));
    item.set("offset", Json(diagnostic.location.byte_offset));
    item.set("tick", Json(diagnostic.location.tick));
    errors.push(std::move(item));
  }
  root.set("errors", std::move(errors));

  Diagnostic error;
  if (!write_json_if_requested(args, "--json", root, &error)) {
    return fail(error);
  }

  print_diagnostics(report.diagnostics);
  std::printf("manifest_roundtrip=%.0f%%\n", percent);
  std::printf("keyframe_hits=%zu/%zu\n", report.keyframe_hits, report.keyframe_total);
  std::printf("tracks=%zu segments=%zu quarantined=%zu\n", report.tracks,
              report.segments, report.quarantined);
  return report.problems == 0 ? 0 : 2;
}

// ---------------------------------------------------------------------------
// restore
// ---------------------------------------------------------------------------

int command_restore(const Args& args) {
  std::string manifest_path;
  if (!args.get("--manifest", &manifest_path) || manifest_path.empty()) {
    return fail(usage_error("restore requires --manifest FILE"));
  }
  RestoreOptions options;
  if (args.has_option("--rate")) {
    int64_t rate = 0;
    if (!args.get_int("--rate", &rate) || rate < 8000 || rate > 768000) {
      return fail(usage_error("--rate must be an integer between 8000 and 768000"));
    }
    options.rate = static_cast<int>(rate);
  }
  std::string layout;
  if (args.has_option("--layout") && !args.get("--layout", &layout)) {
    return fail(usage_error("--layout requires a value"));
  }
  if (!layout.empty()) {
    AVChannelLayout parsed{};
    if (!parse_layout(layout, &parsed)) {
      return fail(usage_error("--layout is not a supported layout: " + layout));
    }
    av_channel_layout_uninit(&parsed);
    options.layout = layout;
  }
  if (args.has_option("--loudness")) {
    double loudness = 0.0;
    if (!args.get_double("--loudness", &loudness) || loudness < -70.0 || loudness > 0.0) {
      return fail(usage_error("--loudness must be between -70 and 0 LUFS"));
    }
    options.loudness = loudness;
  }
  double true_peak = -1.0;
  if (args.has_option("--true-peak")) {
    if (!args.get_double("--true-peak", &true_peak) || true_peak > 0.0 ||
        true_peak < -20.0) {
      return fail(usage_error("--true-peak must be between -20 and 0 dBTP"));
    }
    options.true_peak = true_peak;
  }
  // Archive color values: the policy writes these over conflicting or
  // missing source metadata when --color archive is selected.
  if (args.has_option("--archive-primaries")) {
    if (!args.get("--archive-primaries", &options.archive_color.primaries)) {
      return fail(usage_error("--archive-primaries requires a value"));
    }
  }
  if (args.has_option("--archive-transfer")) {
    if (!args.get("--archive-transfer", &options.archive_color.transfer)) {
      return fail(usage_error("--archive-transfer requires a value"));
    }
  }
  if (args.has_option("--archive-range")) {
    if (!args.get("--archive-range", &options.archive_color.range)) {
      return fail(usage_error("--archive-range requires a value"));
    }
  }
  std::string strategy;
  if (args.get("--color", &strategy)) {
    ColorStrategy parsed = ColorStrategy::archive;
    if (!parse_color_strategy(strategy, &parsed)) {
      return fail(usage_error("--color must be 'preserve' or 'archive'"));
    }
    options.color_strategy = parsed;
  }
  const Limits limits;
  Manifest manifest;
  std::vector<Diagnostic> load_diagnostics;
  if (!load_manifest(manifest_path, limits, &manifest, &load_diagnostics)) {
    return fail(std::move(load_diagnostics));
  }

  // Restored payloads are always written so `verify` has something to hash;
  // --out only relocates them.  The default sits next to the manifest, which
  // keeps the archive self-contained.
  std::string out_dir;
  if (!args.get("--out", &out_dir) || out_dir.empty()) {
    out_dir = (std::filesystem::path(manifest_path).parent_path() / "restored")
                  .string();
  }
  options.output_dir = out_dir;
  options.write_media = true;

  RestoreSummary summary;
  std::vector<Diagnostic> diagnostics;
  const bool ok = restore_archive(manifest, options, limits, &summary, &diagnostics);
  print_diagnostics(diagnostics);

  const Json result = restore_result_json(manifest, summary, options);
  Diagnostic json_error;
  if (!write_json_if_requested(args, "--json", result, &json_error)) {
    return fail(json_error);
  }

  // Terminal summary lines are part of the acceptance contract.
  const size_t roundtripped = manifest_roundtrip_score(manifest_path, manifest);
  const double percent =
      manifest.tracks.empty()
          ? 0.0
          : 100.0 * static_cast<double>(roundtripped) /
                static_cast<double>(manifest.tracks.size());
  size_t hits = 0;
  size_t total = 0;
  size_t hashes = 0;
  size_t hash_total = 0;
  double lufs_sum = 0.0;
  size_t lufs_count = 0;
  for (const auto& segment : summary.segments) {
    if (!segment.quarantined) {
      hits += segment.index_hits;
      total += segment.index_entries;
    }
    if (!segment.quarantined && segment.restored) {
      ++hash_total;
      if (!segment.failure) ++hashes;
    }
    if (!segment.quarantined && segment.audio.loudness_measured &&
        !segment.audio.silent && !segment.audio.short_segment) {
      lufs_sum += segment.audio.output_lufs;
      ++lufs_count;
    }
  }
  const double lufs = lufs_count == 0 ? options.loudness : lufs_sum / lufs_count;
  std::printf("manifest_roundtrip=%.0f%%\n", percent);
  std::printf("keyframe_hits=%zu/%zu\n", hits, total);
  std::printf("segment_hash_match=%zu/%zu\n", hashes, hash_total);
  std::printf("audio_rate_out=%d\n", options.rate);
  std::printf("layout_out=%s\n", options.layout.c_str());
  std::printf("loudness_lufs=%.1f\n", lufs);
  std::printf("segments=%zu failures=%zu quarantined_ok=%zu quarantined_mismatch=%zu\n",
              summary.segments.size(), summary.failures, summary.quarantined_ok,
              summary.quarantined_mismatch);

  if (!ok) {
    if (summary.quarantined_mismatch > 0) return 2;
    if (summary.failures > 0) {
      // Distinguish media validation failures (exit 2) from restore failures.
      for (const auto& segment : summary.segments) {
        if (segment.failure && !segment.quarantined &&
            exit_code_for(*segment.failure) == 2) {
          return 2;
        }
      }
      return 3;
    }
  }
  return 0;
}

// ---------------------------------------------------------------------------
// verify
// ---------------------------------------------------------------------------

int command_verify(const Args& args) {
  std::string manifest_path;
  std::string result_path;
  if (!args.get("--manifest", &manifest_path) || manifest_path.empty()) {
    return fail(usage_error("verify requires --manifest FILE"));
  }
  if (!args.get("--result", &result_path) || result_path.empty()) {
    return fail(usage_error("verify requires --result FILE"));
  }
  const Limits limits;
  Manifest manifest;
  std::vector<Diagnostic> diagnostics;
  if (!load_manifest(manifest_path, limits, &manifest, &diagnostics)) {
    return fail(std::move(diagnostics));
  }
  std::string text;
  Diagnostic error;
  if (!read_file(result_path, limits.max_manifest_bytes, &text, &error)) {
    return fail(error);
  }
  std::string parse_error;
  size_t offset = 0;
  const Json result = Json::parse(text, &parse_error, &offset);
  if (!parse_error.empty()) {
    Diagnostic diagnostic;
    diagnostic.code = ErrorCode::json_parse;
    diagnostic.message = "result: " + parse_error;
    diagnostic.location.file = result_path;
    diagnostic.location.byte_offset = static_cast<int64_t>(offset);
    return fail(diagnostic);
  }

  VerifyOptions options;
  double loudness = -23.0;
  if (args.get_double("--loudness", &loudness)) options.loudness_target = loudness;
  const Json* target = result.find("loudness_target_lufs");
  if (target != nullptr && target->is_number()) {
    options.loudness_target = target->as_number();
  }
  std::string expected_layout;
  if (args.get("--layout", &expected_layout)) options.expected_layout = expected_layout;
  int64_t expected_rate = 0;
  if (args.get_int("--rate", &expected_rate)) {
    options.expected_rate = static_cast<int>(expected_rate);
  }

  VerifySummary summary;
  std::vector<Diagnostic> verify_diagnostics;
  const bool ok = verify_archive(manifest, result, options, limits, &summary,
                                 &verify_diagnostics);
  print_diagnostics(verify_diagnostics);

  const size_t roundtripped = manifest_roundtrip_score(manifest_path, manifest);
  const double percent =
      manifest.tracks.empty()
          ? 0.0
          : 100.0 * static_cast<double>(roundtripped) /
                static_cast<double>(manifest.tracks.size());
  std::printf("manifest_roundtrip=%.0f%%\n", percent);
  std::printf("keyframe_hits=%zu/%zu\n", summary.keyframe_hits,
              summary.keyframe_total);
  const size_t hash_total =
      summary.hash_total == 0 ? summary.manifest_segments : summary.hash_total;
  std::printf("segment_hash_match=%zu/%zu\n", summary.hash_matches, hash_total);
  std::printf("audio_rate_out=%d\n", summary.audio_rate_out);
  std::printf("layout_out=%s\n", summary.layout_out.c_str());
  std::printf("loudness_lufs=%.1f\n", summary.loudness_lufs);
  std::printf("segments=%zu quarantined_ok=%zu/%zu failures=%zu\n",
              summary.manifest_segments, summary.quarantined_ok,
              summary.quarantined_expected, summary.failures);

  std::string json_path;
  if (args.get("--json", &json_path)) {
    Json report = Json::object();
    report.set("format", Json("media-archive-verify-report"));
    report.set("version", Json(int64_t{1}));
    report.set("manifest_roundtrip_percent", Json(percent));
    report.set("keyframe_hits", Json(static_cast<int64_t>(summary.keyframe_hits)));
    report.set("keyframe_total", Json(static_cast<int64_t>(summary.keyframe_total)));
    report.set("segment_hash_match",
               Json(static_cast<int64_t>(summary.hash_matches)));
    report.set("segment_hash_total", Json(static_cast<int64_t>(hash_total)));
    report.set("audio_rate_out", Json(static_cast<int64_t>(summary.audio_rate_out)));
    report.set("layout_out", Json(summary.layout_out));
    report.set("loudness_lufs", Json(summary.loudness_lufs));
    report.set("quarantined_ok",
               Json(static_cast<int64_t>(summary.quarantined_ok)));
    report.set("quarantined_expected",
               Json(static_cast<int64_t>(summary.quarantined_expected)));
    report.set("failures", Json(static_cast<int64_t>(summary.failures)));
    report.set("passed", Json(ok));
    Diagnostic write_error;
    if (!write_file_atomic(json_path, report.dump(), &write_error)) {
      return fail(write_error);
    }
  }
  return ok ? 0 : 2;
}

}  // namespace

int main(int argc, char** argv) {
  Args args;
  Diagnostic error;
  if (!parse_args(argc, argv, &args, &error)) {
    print_diagnostics({error});
    usage(std::cerr);
    return exit_code_for(error.code);
  }
  if (args.command == "--help" || args.command == "help" || args.has("--help")) {
    usage(std::cout);
    return 0;
  }
  if (args.command == "generate") return command_generate(args);
  if (args.command == "inspect") return command_inspect(args);
  if (args.command == "restore") return command_restore(args);
  if (args.command == "verify") return command_verify(args);

  std::cerr << "error code=E_USAGE message=unknown command: " << args.command
            << '\n';
  usage(std::cerr);
  return 1;
}

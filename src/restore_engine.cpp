#include "restore_engine.hpp"

#include <algorithm>
#include <cstdio>
#include <filesystem>
#include <map>
#include <set>
#include <sstream>

#include "file_io.hpp"
#include "sha256.hpp"

namespace media_restore {
namespace {

Diagnostic make(ErrorCode code, std::string message, Location location = {},
                Severity severity = Severity::error) {
  Diagnostic diagnostic;
  diagnostic.code = code;
  diagnostic.severity = severity;
  diagnostic.message = std::move(message);
  diagnostic.location = std::move(location);
  return diagnostic;
}

// Canonical description of everything that determines a segment's output.
// Restore and verify build this string identically, so a hash match proves
// the timeline, index and audio agree rather than that two copies of a number
// agree.
class SummaryHash {
 public:
  void field(std::string_view name, std::string_view value) {
    sha_.update(name);
    sha_.update("=");
    sha_.update(value);
    sha_.update(";");
  }
  void field(std::string_view name, int64_t value) {
    field(name, std::to_string(value));
  }
  void field(std::string_view name, double value) {
    char buffer[64];
    std::snprintf(buffer, sizeof(buffer), "%.6f", value);
    field(name, std::string(buffer));
  }
  std::string hex() { return sha_.hex(); }

 private:
  Sha256 sha_;
};

Location segment_location(const std::string& track, const std::string& segment) {
  Location location;
  location.track = track;
  location.segment = segment;
  return location;
}

struct Validated {
  StreamScan scan;
  IndexReport index;
  std::string media_rel;
  std::string media_sha;
  int64_t media_bytes = 0;
};

// Maps an index report onto per-segment counters for output.
void fill_index_counts(const IndexReport& report, SegmentOutcome* outcome) {
  outcome->index_entries = report.index_entries;
  outcome->index_hits = report.hits;
  outcome->index_generated = report.generated;
  outcome->scan_keyframes = report.scan_keyframes;
}

bool validate_segment(const Manifest& manifest, const Segment& segment,
                      const Limits& limits, Validated* out,
                      std::vector<Diagnostic>* diagnostics) {
  const size_t before = diagnostics->size();
  const Location loc = segment_location(segment.id, segment.id);
  std::filesystem::path media_path;
  Diagnostic error;
  if (!resolve_archive_path(manifest.source_path, segment.media, &media_path,
                            &error)) {
    diagnostics->push_back(std::move(error));
    return false;
  }
  int64_t size = 0;
  if (!file_size(media_path, &size, &error)) {
    diagnostics->push_back(std::move(error));
    return false;
  }
  if (size > limits.max_media_bytes) {
    diagnostics->push_back(make(ErrorCode::limit_exceeded,
                                "media file exceeds the archive limit", loc));
    return false;
  }
  // Content must agree with the declared codec marker.  An empty file or an
  // unrecognised signature is reported at offset 0 of the offending file.
  std::string prefix;
  if (!read_prefix(media_path, 0, 64, &prefix, &error)) {
    diagnostics->push_back(std::move(error));
    return false;
  }
  Codec sniffed = Codec::h264;
  Location where = loc;
  where.file = media_path.string();
  where.byte_offset = 0;
  if (prefix.empty()) {
    diagnostics->push_back(make(ErrorCode::unknown_codec,
                                "media file is empty", where));
    return false;
  }
  if (!sniff_codec(prefix, &sniffed)) {
    diagnostics->push_back(make(
        ErrorCode::unknown_codec,
        "content does not match any supported raw stream codec", where));
    return false;
  }
  if (sniffed != segment.codec) {
    diagnostics->push_back(make(
        ErrorCode::unknown_codec,
        "declared codec '" + std::string(codec_name(segment.codec)) +
            "' does not match the content ('" + codec_name(sniffed) + "')",
        where));
    return false;
  }
  if (!scan_raw_stream(media_path, segment.codec, limits, &out->scan,
                       diagnostics)) {
    return false;
  }

  // Index entries come from the manifest when present, otherwise from the
  // external index file.  The manifest version always governs; a mismatch is
  // a schema-level disagreement, not a silent fallback.
  KeyframeIndex index = segment.index;
  if (index.present && index.entries.empty() && !index.path.empty()) {
    std::filesystem::path index_path;
    if (!resolve_archive_path(manifest.source_path, index.path, &index_path,
                              &error)) {
      diagnostics->push_back(std::move(error));
      return false;
    }
    KeyframeIndex loaded;
    if (!load_keyframe_index(index_path, limits, &loaded, diagnostics)) {
      return false;
    }
    if (loaded.version != index.version) {
      diagnostics->push_back(make(ErrorCode::index_version,
                                  "index version in the manifest (" +
                                      std::to_string(index.version) +
                                      ") differs from the index file (" +
                                      std::to_string(loaded.version) + ")",
                                  loc));
      return false;
    }
    index.entries = std::move(loaded.entries);
  }
  if (!validate_keyframe_index(index, out->scan, limits, &out->index,
                               diagnostics)) {
    return false;
  }
  if (!hash_file(media_path, &out->media_sha, &error)) {
    diagnostics->push_back(std::move(error));
    return false;
  }
  out->media_rel = segment.media;
  out->media_bytes = size;
  for (size_t i = before; i < diagnostics->size(); ++i) {
    if ((*diagnostics)[i].is_error()) return false;
  }
  return true;
}

Json resolved_field_json(const ResolvedField& field) {
  Json json = Json::object();
  if (field.value) {
    json.set("value", Json(*field.value));
  } else {
    json.set("value", Json(nullptr));
  }
  json.set("source", Json(field.source));
  json.set("missing_from_segment", Json(field.missing_from_segment));
  json.set("applied_archive", Json(field.applied_archive));
  Json conflicts = Json::array();
  for (const auto& value : field.conflicts) conflicts.push(Json(value));
  json.set("conflicts", std::move(conflicts));
  return json;
}

Json color_json(const SegmentColor& color) {
  Json json = Json::object();
  json.set("primaries", resolved_field_json(color.primaries));
  json.set("transfer", resolved_field_json(color.transfer));
  json.set("matrix", resolved_field_json(color.matrix));
  json.set("range", resolved_field_json(color.range));
  json.set("conflicted", Json(color.conflicted));
  json.set("missing", Json(color.missing_any));
  return json;
}

Json audio_json(const AudioStats& stats) {
  Json json = Json::object();
  json.set("input_rate", Json(static_cast<int64_t>(stats.input_rate)));
  json.set("output_rate", Json(static_cast<int64_t>(stats.output_rate)));
  json.set("layout_in", Json(stats.input_layout));
  json.set("layout_out", Json(stats.output_layout));
  json.set("frames_in", Json(stats.input_frames));
  json.set("frames_out", Json(stats.output_frames));
  json.set("discarded_frames", Json(stats.discarded_frames));
  json.set("padded_frames", Json(stats.padded_frames));
  json.set("resampler_delay", Json(stats.resampler_delay));
  json.set("input_lufs", Json(stats.input_lufs));
  json.set("output_lufs", Json(stats.output_lufs));
  json.set("input_true_peak_dbtp", Json(stats.input_true_peak_dbtp));
  json.set("output_true_peak_dbtp", Json(stats.output_true_peak_dbtp));
  json.set("limited_samples", Json(static_cast<int64_t>(stats.limited_samples)));
  json.set("applied_gain_db", Json(stats.applied_gain_db));
  json.set("offset_rounding_seconds", Json(stats.offset_rounding_seconds));
  json.set("silent", Json(stats.silent));
  json.set("short_segment", Json(stats.short_segment));
  json.set("loudness_measured", Json(stats.loudness_measured));
  return json;
}

std::string output_name(const std::string& track, const Segment& segment) {
  return track + "-" + segment.id + ".wav";
}

bool load_existing_hash(const std::filesystem::path& path, std::string* hex,
                        Diagnostic* error) {
  std::string text;
  if (!read_file(path, 1 << 20, &text, error)) return false;
  *hex = std::move(text);
  return true;
}

}  // namespace

bool restore_archive(const Manifest& manifest, const RestoreOptions& options,
                     const Limits& limits, RestoreSummary* summary,
                     std::vector<Diagnostic>* diagnostics) {
  const size_t before = diagnostics->size();
  *summary = RestoreSummary{};
  summary->time_base = manifest.time_base;

  // Structural checks first: the timeline and dependency graph must be sound
  // before any per-segment work is attempted.
  Timeline timeline;
  std::vector<Diagnostic> structural;
  build_timeline(manifest, &structural, &timeline);
  summary->track_order = timeline.order.order;
  summary->cyclic_tracks = timeline.order.cyclic;
  summary->blocked_tracks = timeline.order.blocked;
  // Structural diagnostics that name a segment are held back and attached to
  // that segment's outcome; everything else is archive-level.
  std::map<std::pair<std::string, std::string>, std::vector<Diagnostic>>
      structural_by_segment;
  bool structure_ok = true;
  for (auto& diagnostic : structural) {
    const Segment* owner =
        manifest.find_segment(diagnostic.location.track,
                              diagnostic.location.segment);
    // A quarantined segment is allowed to break structural rules that its
    // manifest entry declares as expected; anything else is fatal.
    if (owner != nullptr && owner->expected_code &&
        (diagnostic.code == ErrorCode::segment_overlap ||
         diagnostic.code == ErrorCode::segment_empty ||
         diagnostic.code == ErrorCode::segment_negative ||
         diagnostic.code == ErrorCode::manifest_schema)) {
      structural_by_segment[{diagnostic.location.track,
                             diagnostic.location.segment}]
          .push_back(std::move(diagnostic));
      continue;
    }
    if (!diagnostic.location.segment.empty()) {
      structural_by_segment[{diagnostic.location.track,
                             diagnostic.location.segment}]
          .push_back(std::move(diagnostic));
      continue;
    }
    if (diagnostic.is_error()) structure_ok = false;
    diagnostics->push_back(std::move(diagnostic));
  }
  if (!structure_ok) {
    // A broken timeline cannot be restored; report and stop before doing
    // partial work that might overwrite valid results.
    summary->failures += 1;
    return false;
  }

  std::set<std::string> resolve_order(timeline.order.order.begin(),
                                      timeline.order.order.end());
  std::set<std::string> blocked(timeline.order.blocked.begin(),
                                timeline.order.blocked.end());
  std::set<std::string> cyclic(timeline.order.cyclic.begin(),
                               timeline.order.cyclic.end());

  // Restoration follows the topological order so a dependent track always
  // sees its dependencies' outcomes.  Tracks that could not be ordered run
  // last; they are marked skipped regardless, but keeping them out of the
  // ordered pass makes the invariant explicit.
  std::vector<const Track*> execution_order;
  std::set<std::string> scheduled;
  for (const auto& id : timeline.order.order) {
    const Track* track = manifest.find_track(id);
    if (track != nullptr && scheduled.insert(id).second) {
      execution_order.push_back(track);
    }
  }
  for (const auto& track : manifest.tracks) {
    if (scheduled.insert(track.id).second) execution_order.push_back(&track);
  }

  std::map<std::string, bool> track_restored;
  for (const Track* track_ptr : execution_order) {
    const Track& track = *track_ptr;
    const TrackTimeline* view = timeline.find(track.id);
    if (view == nullptr) continue;
    bool track_can_restore = resolve_order.contains(track.id) &&
                             !blocked.contains(track.id) &&
                             !cyclic.contains(track.id);
    for (size_t i = 0; i < track.segments.size(); ++i) {
      const Segment& segment = track.segments[i];
      SegmentOutcome outcome;
      outcome.track_id = track.id;
      outcome.segment_id = segment.id;
      outcome.quarantined = segment.expected_code.has_value();

      std::vector<Diagnostic> segment_diagnostics;
      auto held = structural_by_segment.find({track.id, segment.id});
      if (held != structural_by_segment.end()) {
        segment_diagnostics = std::move(held->second);
      }
      Validated validated;
      const bool validated_ok =
          track.kind == TrackKind::audio
              ? true
              : validate_segment(manifest, segment, limits, &validated,
                                 &segment_diagnostics);
      fill_index_counts(validated.index, &outcome);
      outcome.stream_bytes = validated.scan.stream_size;
      outcome.validated = validated_ok;

      // Apply color resolution per segment; conflicts are warnings.
      SegmentColor resolved_color;
      resolve_segment_color(track, segment, options.color_strategy,
                            options.archive_color, &resolved_color,
                            &segment_diagnostics);
      outcome.color = resolved_color;

      // Decide the fate of this segment.
      if (outcome.quarantined) {
        const ErrorCode expected = *segment.expected_code;
        bool matched = false;
        for (const auto& diagnostic : segment_diagnostics) {
          if (diagnostic.severity == Severity::error &&
              diagnostic.code == expected) {
            matched = true;
          }
        }
        if (matched) {
          ++summary->quarantined_ok;
          outcome.failure = expected;
          outcome.failure_message = "quarantine confirmed";
        } else {
          ++summary->quarantined_mismatch;
          ++summary->failures;
          outcome.failure = ErrorCode::internal;
          outcome.failure_message =
              "expected " + std::string(error_code_name(expected)) +
              " but the segment did not fail that way";
        }
      } else if (!validated_ok) {
        // Keep only the first fatal error for the segment's own record; all
        // diagnostics still reach stderr through `diagnostics`.
        for (const auto& diagnostic : segment_diagnostics) {
          if (diagnostic.severity == Severity::error) {
            outcome.failure = diagnostic.code;
            outcome.failure_message = diagnostic.message;
            break;
          }
        }
        if (!outcome.failure) {
          outcome.failure = ErrorCode::internal;
          outcome.failure_message = "segment validation failed";
        }
        ++summary->failures;
      }

      // Restoration only for valid, non-quarantined segments whose track can
      // be ordered.  Independent tracks continue after a sibling's failure.
      const bool should_restore =
          outcome.validated && !outcome.quarantined && options.write_media;
      if (should_restore) {
        bool dependency_failed = false;
        for (const auto& dependency : track.depends_on) {
          const auto found = track_restored.find(dependency);
          // A dependency that has not run yet (unorderable) or that failed
          // both block this track: silence must never look like success.
          if (found == track_restored.end() || !found->second) {
            dependency_failed = true;
          }
        }
        if (!track_can_restore) {
          outcome.skipped = true;
          outcome.failure = ErrorCode::dependency_failed;
          outcome.failure_message =
              cyclic.contains(track.id)
                  ? "track is part of a dependency cycle"
                  : (blocked.contains(track.id)
                         ? "track has an unresolvable dependency"
                         : "track cannot be ordered");
          ++summary->failures;
        } else if (dependency_failed) {
          outcome.skipped = true;
          outcome.failure = ErrorCode::dependency_failed;
          outcome.failure_message = "a dependency track failed to restore";
          ++summary->failures;
        } else if (segment.audio && options.loudness_enabled) {
          std::filesystem::path media_path;
          Diagnostic error;
          AudioRestoreConfig config;
          config.output_rate = options.rate;
          config.target_lufs = options.loudness;
          config.true_peak_dbtp = options.true_peak;
          if (!parse_layout(options.layout, &config.output_layout)) {
            segment_diagnostics.push_back(
                make(ErrorCode::audio_unsupported_layout,
                     "unsupported target layout '" + options.layout + "'",
                     segment_location(track.id, segment.id)));
          } else if (!resolve_archive_path(
                         manifest.source_path,
                         segment.audio->media.empty() ? segment.media
                                                      : segment.audio->media,
                         &media_path, &error)) {
            segment_diagnostics.push_back(std::move(error));
          } else {
            const std::filesystem::path out_dir =
                options.output_dir / track.id;
            std::filesystem::path output_path = out_dir / output_name(track.id, segment);
            RestoredAudio restored;
            if (!restore_segment_audio(media_path, segment.audio->offset,
                                       manifest.time_base, config, &restored,
                                       &segment_diagnostics)) {
              outcome.failure = segment_diagnostics.back().code;
              outcome.failure_message = segment_diagnostics.back().message;
              ++summary->failures;
            } else {
              // A failing segment must never clobber a previously valid file,
              // and a changed configuration must not silently keep the old
              // payload: reuse only when the computed hash equals the recorded
              // one and the file on disk still matches it.
              Diagnostic write_error;
              const std::filesystem::path hash_path =
                  output_path.string() + ".sha256";
              const std::string computed = Sha256::digest(restored.wav);
              std::string existing;
              bool reused = false;
              if (load_existing_hash(hash_path, &existing, &write_error) &&
                  existing == computed && std::filesystem::exists(output_path)) {
                std::string actual;
                if (sha256_file(output_path, &actual, &write_error) &&
                    actual == existing) {
                  reused = true;
                  outcome.segment_hash = existing;
                  outcome.hash_reused = true;
                }
              }
              if (!reused) {
                outcome.segment_hash = computed;
                if (!write_file_atomic(output_path, restored.wav, &write_error) ||
                    !write_file_atomic(hash_path, outcome.segment_hash, &write_error)) {
                  outcome.failure = write_error.code;
                  outcome.failure_message = write_error.message;
                  ++summary->failures;
                } else {
                  outcome.restored = true;
                  outcome.output_path = output_path.string();
                  outcome.audio = restored.stats;
                }
              } else {
                outcome.restored = true;
                outcome.output_path = output_path.string();
                outcome.audio = restored.stats;
              }
              // The hash must be reproducible: a second run over unchanged
              // inputs produces the same payload.  A reused hash is compared
              // against the file on disk, not recomputed from this run's
              // buffer (which the earlier restore already matched).
              if (outcome.restored && !reused &&
                  outcome.segment_hash != Sha256::digest(restored.wav)) {
                outcome.failure = ErrorCode::internal;
                outcome.failure_message =
                    "restored payload hash is not reproducible";
                ++summary->failures;
              }
            }
          }
        }
      }

      // A stable digest of everything that produced this segment: timeline
      // position, index provenance, bitstream identity and audio outcome.
      {
        const TimelineSegment* span = nullptr;
        for (const auto& candidate : view->segments) {
          if (candidate.segment_id == segment.id) span = &candidate;
        }
        SummaryHash digest;
        digest.field("codec", codec_name(segment.codec));
        digest.field("media", validated.media_rel);
        digest.field("media_sha256", validated.media_sha);
        digest.field("start", span ? span->start_tick : int64_t{0});
        digest.field("duration", span ? span->duration_tick : int64_t{0});
        digest.field("index_entries", static_cast<int64_t>(outcome.index_entries));
        digest.field("index_hits", static_cast<int64_t>(outcome.index_hits));
        digest.field("index_generated", static_cast<int64_t>(outcome.index_generated));
        digest.field("scan_keyframes", static_cast<int64_t>(outcome.scan_keyframes));
        digest.field("audio_frames_out", outcome.audio.output_frames);
        digest.field("audio_rate", static_cast<int64_t>(outcome.audio.output_rate));
        digest.field("audio_layout", outcome.audio.output_layout);
        digest.field("loudness", outcome.audio.output_lufs);
        digest.field("padded", outcome.audio.padded_frames);
        digest.field("discarded", outcome.audio.discarded_frames);
        digest.field("segment_sha256", outcome.segment_hash);
        outcome.summary_hash = digest.hex();
      }
      track_restored[track.id] =
          track_restored[track.id] || !outcome.failure.has_value();
      for (auto& diagnostic : segment_diagnostics) {
        if (outcome.quarantined) {
          diagnostic.location.track = track.id;
          diagnostic.location.segment = segment.id;
        }
        diagnostics->push_back(std::move(diagnostic));
      }
      summary->segments.push_back(std::move(outcome));
    }
  }

  // Healthy success accounting and overall failure count.
  for (const auto& outcome : summary->segments) {
    if (!outcome.quarantined) {
      ++summary->healthy_segments;
      if (outcome.restored && !outcome.failure) ++summary->healthy_hash_matches;
    }
  }
  // Quarantined segments legitimately emit the errors the manifest declares;
  // only unaccounted-for errors are archive-level failures.
  for (size_t i = before; i < diagnostics->size(); ++i) {
    const Diagnostic& diagnostic = (*diagnostics)[i];
    if (!diagnostic.is_error()) continue;
    const Segment* owner = manifest.find_segment(diagnostic.location.track,
                                                 diagnostic.location.segment);
    if (owner != nullptr && owner->expected_code == diagnostic.code) continue;
    ++summary->failures;
  }
  return summary->failures == 0;
}

Json restore_result_json(const Manifest& /*manifest*/, const RestoreSummary& summary,
                         const RestoreOptions& options) {
  Json root = Json::object();
  root.set("format", Json("media-archive-restore-result"));
  root.set("version", Json(int64_t{1}));
  Json time_base = Json::object();
  time_base.set("num", Json(std::to_string(summary.time_base.num)));
  time_base.set("den", Json(std::to_string(summary.time_base.den)));
  root.set("time_base", std::move(time_base));
  Json order = Json::array();
  for (const auto& id : summary.track_order) order.push(Json(id));
  root.set("track_order", std::move(order));
  Json cyclic = Json::array();
  for (const auto& id : summary.cyclic_tracks) cyclic.push(Json(id));
  root.set("cyclic_tracks", std::move(cyclic));
  Json blocked = Json::array();
  for (const auto& id : summary.blocked_tracks) blocked.push(Json(id));
  root.set("blocked_tracks", std::move(blocked));

  size_t keyframe_hits = 0;
  size_t keyframe_total = 0;
  size_t quarantined_hits = 0;
  size_t quarantined_total = 0;
  for (const auto& outcome : summary.segments) {
    if (outcome.quarantined) {
      quarantined_hits += outcome.index_hits;
      quarantined_total += outcome.index_entries;
    } else {
      keyframe_hits += outcome.index_hits;
      keyframe_total += outcome.index_entries;
    }
  }
  root.set("index_hits", Json(static_cast<int64_t>(keyframe_hits)));
  root.set("index_total", Json(static_cast<int64_t>(keyframe_total)));
  root.set("quarantined_index_hits",
           Json(static_cast<int64_t>(quarantined_hits)));
  root.set("quarantined_index_total",
           Json(static_cast<int64_t>(quarantined_total)));
  root.set("audio_rate_out", Json(static_cast<int64_t>(options.rate)));
  root.set("layout_out", Json(options.layout));
  root.set("loudness_target_lufs", Json(options.loudness));
  root.set("color_strategy", Json(color_strategy_name(options.color_strategy)));

  Json segments = Json::array();
  for (const auto& outcome : summary.segments) {
    Json item = Json::object();
    item.set("track", Json(outcome.track_id));
    item.set("segment", Json(outcome.segment_id));
    item.set("quarantined", Json(outcome.quarantined));
    item.set("validated", Json(outcome.validated));
    item.set("restored", Json(outcome.restored));
    item.set("skipped", Json(outcome.skipped));
    item.set("index_entries", Json(static_cast<int64_t>(outcome.index_entries)));
    item.set("index_hits", Json(static_cast<int64_t>(outcome.index_hits)));
    item.set("index_generated", Json(static_cast<int64_t>(outcome.index_generated)));
    item.set("scan_keyframes", Json(static_cast<int64_t>(outcome.scan_keyframes)));
    item.set("stream_bytes", Json(outcome.stream_bytes));
    item.set("segment_hash", Json(outcome.segment_hash));
    item.set("summary_hash", Json(outcome.summary_hash));
    item.set("hash_reused", Json(outcome.hash_reused));
    item.set("output_path", Json(outcome.output_path));
    if (outcome.failure) {
      item.set("failure_code", Json(error_code_name(*outcome.failure)));
      item.set("failure_message", Json(outcome.failure_message));
    } else {
      item.set("failure_code", Json(nullptr));
    }
    item.set("color", color_json(outcome.color));
    item.set("audio", audio_json(outcome.audio));
    segments.push(std::move(item));
  }
  root.set("segments", std::move(segments));

  Json totals = Json::object();
  totals.set("segments", Json(static_cast<int64_t>(summary.segments.size())));
  totals.set("healthy", Json(static_cast<int64_t>(summary.healthy_segments)));
  totals.set("healthy_hash_matches",
             Json(static_cast<int64_t>(summary.healthy_hash_matches)));
  totals.set("quarantined_ok", Json(static_cast<int64_t>(summary.quarantined_ok)));
  totals.set("quarantined_mismatch",
             Json(static_cast<int64_t>(summary.quarantined_mismatch)));
  totals.set("failures", Json(static_cast<int64_t>(summary.failures)));
  root.set("totals", std::move(totals));
  return root;
}

bool verify_archive(const Manifest& manifest, const Json& result,
                    const VerifyOptions& options, const Limits& limits,
                    VerifySummary* summary,
                    std::vector<Diagnostic>* diagnostics) {
  const size_t before = diagnostics->size();
  *summary = VerifySummary{};
  if (!result.is_object()) {
    diagnostics->push_back(make(ErrorCode::json_parse,
                                "result document must be a JSON object"));
    return false;
  }
  const Json* segments = result.find("segments");
  if (segments == nullptr || !segments->is_array()) {
    diagnostics->push_back(make(ErrorCode::manifest_schema,
                                "result document needs a segments array"));
    return false;
  }
  const Json* rate = result.find("audio_rate_out");
  if (rate != nullptr && rate->is_integer()) {
    summary->audio_rate_out = static_cast<int>(rate->as_integer());
  }
  const Json* layout = result.find("layout_out");
  if (layout != nullptr && layout->is_string()) {
    summary->layout_out = layout->as_string();
  }
  const int rate_expected =
      options.expected_rate > 0 ? options.expected_rate : summary->audio_rate_out;
  const std::string layout_expected =
      options.expected_layout.empty() ? summary->layout_out : options.expected_layout;

  // Cache scans per media file: segments often share a raw stream.
  std::map<std::string, StreamScan> scan_cache;
  std::map<std::string, std::string> hash_cache;

  for (const auto& item : segments->items()) {
    if (!item.is_object()) {
      diagnostics->push_back(make(ErrorCode::manifest_schema,
                                  "result segment must be an object"));
      continue;
    }
    const Json* track = item.find("track");
    const Json* segment_id = item.find("segment");
    if (track == nullptr || !track->is_string() || segment_id == nullptr ||
        !segment_id->is_string()) {
      diagnostics->push_back(make(ErrorCode::manifest_schema,
                                  "result segment needs track and segment ids"));
      continue;
    }
    ++summary->manifest_segments;
    const Segment* segment =
        manifest.find_segment(track->as_string(), segment_id->as_string());
    if (segment == nullptr) {
      diagnostics->push_back(make(
          ErrorCode::manifest_schema,
          "result references a segment that is not in the manifest",
          segment_location(track->as_string(), segment_id->as_string())));
      ++summary->failures;
      continue;
    }
    ++summary->manifest_roundtripped;

    const bool quarantined = segment->expected_code.has_value();
    const Json* failure = item.find("failure_code");
    std::string failure_code;
    if (failure != nullptr && failure->is_string()) failure_code = failure->as_string();
    const Json* validated = item.find("validated");
    const bool was_validated = validated != nullptr && validated->is_bool() &&
                               validated->as_bool();

    if (quarantined) {
      ++summary->quarantined_expected;
      if (failure_code == error_code_name(*segment->expected_code)) {
        ++summary->quarantined_ok;
      } else {
        diagnostics->push_back(make(
            ErrorCode::hash_mismatch,
            "quarantined segment did not report the expected " +
                std::string(error_code_name(*segment->expected_code)) +
                " (got '" + failure_code + "')",
            segment_location(track->as_string(), segment->id)));
        ++summary->failures;
      }
      continue;
    }
    if (!was_validated) {
      diagnostics->push_back(make(ErrorCode::hash_mismatch,
                                  "healthy segment was not validated",
                                  segment_location(track->as_string(), segment->id)));
      ++summary->failures;
      continue;
    }

    std::filesystem::path media_path;
    Diagnostic error;
    if (!resolve_archive_path(manifest.source_path, segment->media, &media_path,
                              &error)) {
      diagnostics->push_back(std::move(error));
      ++summary->failures;
      continue;
    }
    // Recompute the index hit count from the actual bitstream, never from the
    // number the result document reports.
    KeyframeIndex index = segment->index;
    if (index.present && index.entries.empty() && !index.path.empty()) {
      std::filesystem::path index_path;
      if (resolve_archive_path(manifest.source_path, index.path, &index_path,
                               &error) &&
          load_keyframe_index(index_path, limits, &index, diagnostics)) {
        // entries now populated
      }
    }
    const bool audio_only = segment->codec == Codec::h264 && !index.present &&
                            segment->audio.has_value() &&
                            segment->media.ends_with(".wav");
    if (!audio_only) {
      const std::string cache_key = media_path.string();
      if (!scan_cache.contains(cache_key)) {
        StreamScan scan;
        if (!scan_raw_stream(media_path, segment->codec, limits, &scan,
                             diagnostics)) {
          ++summary->failures;
          continue;
        }
        scan_cache.emplace(cache_key, std::move(scan));
      }
      const StreamScan& scan = scan_cache[cache_key];
      IndexReport report;
      std::vector<Diagnostic> index_diagnostics;
      validate_keyframe_index(index, scan, limits, &report, &index_diagnostics);
      summary->keyframe_hits += report.hits;
      summary->keyframe_total += report.index_entries;
    }

    // Hash verification: the declared hash must match the file on disk.
    const Json* hash_field = item.find("segment_hash");
    if (hash_field == nullptr || !hash_field->is_string() ||
        hash_field->as_string().empty()) {
      if (item.find("restored") != nullptr &&
          item.find("restored")->is_bool() &&
          item.find("restored")->as_bool()) {
        diagnostics->push_back(make(ErrorCode::hash_mismatch,
                                    "restored segment has no hash",
                                    segment_location(track->as_string(), segment->id)));
        ++summary->failures;
      }
      continue;
    }
    const Json* output_path = item.find("output_path");
    if (output_path == nullptr || !output_path->is_string()) {
      diagnostics->push_back(make(ErrorCode::hash_mismatch,
                                  "restored segment has no output path",
                                  segment_location(track->as_string(), segment->id)));
      ++summary->failures;
      continue;
    }
    ++summary->hash_total;
    std::string actual;
    if (!sha256_file(output_path->as_string(), &actual, &error)) {
      diagnostics->push_back(make(ErrorCode::hash_mismatch,
                                  "cannot read restored media: " + error.message,
                                  segment_location(track->as_string(), segment->id)));
      ++summary->failures;
      continue;
    }
    if (actual != hash_field->as_string()) {
      diagnostics->push_back(make(ErrorCode::hash_mismatch,
                                  "restored payload hash does not match",
                                  segment_location(track->as_string(), segment->id)));
      ++summary->failures;
      continue;
    }
    ++summary->hash_matches;

    const Json* audio = item.find("audio");
    if (audio != nullptr && audio->is_object() && rate_expected > 0) {
      const Json* out_rate = audio->find("output_rate");
      const Json* out_layout = audio->find("layout_out");
      if (out_rate == nullptr || !out_rate->is_integer() ||
          out_rate->as_integer() != rate_expected) {
        diagnostics->push_back(make(ErrorCode::audio_unsupported_rate,
                                    "restored audio rate is not " +
                                        std::to_string(rate_expected),
                                    segment_location(track->as_string(), segment->id)));
        ++summary->failures;
      }
      if (out_layout == nullptr || !out_layout->is_string() ||
          out_layout->as_string() != layout_expected) {
        diagnostics->push_back(make(ErrorCode::audio_unsupported_layout,
                                    "restored audio layout is not '" +
                                        layout_expected + "'",
                                    segment_location(track->as_string(), segment->id)));
        ++summary->failures;
      }
      const Json* silent = audio->find("silent");
      const bool declared_silent =
          silent != nullptr && silent->is_bool() && silent->as_bool();
      // Re-measure the file on disk: a result document is evidence only if
      // the delivered payload really carries the declared loudness.
      PcmBuffer measured_audio;
      std::vector<Diagnostic> measure_diagnostics;
      if (decode_audio_file(output_path->as_string(), limits, &measured_audio,
                            &measure_diagnostics)) {
        if (measured_audio.sample_rate != rate_expected) {
          diagnostics->push_back(make(
              ErrorCode::audio_unsupported_rate,
              "restored file is " + std::to_string(measured_audio.sample_rate) +
                  " Hz, expected " + std::to_string(rate_expected),
              segment_location(track->as_string(), segment->id)));
          ++summary->failures;
        }
        const std::string actual_layout = layout_name(measured_audio.layout);
        if (!layout_expected.empty() && actual_layout != layout_expected) {
          diagnostics->push_back(make(
              ErrorCode::audio_unsupported_layout,
              "restored file layout is '" + actual_layout + "', expected '" +
                  layout_expected + "'",
              segment_location(track->as_string(), segment->id)));
          ++summary->failures;
        }
        double measured_lufs = 0.0;
        double measured_peak = 0.0;
        bool measured_silent = false;
        if (measure_loudness(measured_audio, &measured_lufs, &measured_peak,
                             &measured_silent, &measure_diagnostics)) {
          if (!measured_silent) {
            summary->loudness_sum += measured_lufs;
            ++summary->loudness_samples;
            summary->loudness_lufs = measured_lufs;
          }
          summary->loudness_checked = true;
          if (!measured_silent && !declared_silent &&
              std::fabs(measured_lufs - options.loudness_target) >
                  options.loudness_tolerance) {
            diagnostics->push_back(make(
                ErrorCode::loudness_invalid,
                "restored loudness " + std::to_string(measured_lufs) +
                    " LUFS is outside the target tolerance",
                segment_location(track->as_string(), segment->id),
                Severity::warning));
          }
        }
      }
    }
  }
  if (summary->loudness_samples > 0) {
    summary->loudness_lufs = summary->loudness_sum /
                             static_cast<double>(summary->loudness_samples);
  }
  for (size_t i = before; i < diagnostics->size(); ++i) {
    if ((*diagnostics)[i].is_error()) ++summary->failures;
  }
  return summary->failures == 0;
}

size_t manifest_roundtrip_score(const std::filesystem::path& path,
                               const Manifest& manifest) {
  std::string text;
  Diagnostic error;
  Limits limits;
  if (!read_file(path, limits.max_manifest_bytes, &text, &error)) return 0;
  Manifest reparsed;
  std::vector<Diagnostic> diagnostics;
  if (!parse_manifest(text, &reparsed, &diagnostics)) return 0;
  if (serialize_manifest(reparsed) != serialize_manifest(manifest)) return 0;
  return manifest.tracks.size();
}

}  // namespace media_restore

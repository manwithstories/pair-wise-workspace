#include "relay/pipeline.hpp"

#include <algorithm>
#include <cmath>
#include <fstream>
#include <limits>
#include <set>
#include <sstream>

#include "relay/checkpoint.hpp"
#include "relay/fixture.hpp"
#include "relay/frame_reader.hpp"
#include "relay/hash.hpp"
#include "relay/ladder.hpp"
#include "relay/manifest.hpp"
#include "relay/packet_reorder.hpp"
#include "relay/segmenter.hpp"
#include "relay/stage_cache.hpp"
#include "relay/timeline.hpp"

namespace relay {
namespace {

constexpr std::uint64_t kBitrateStep = 1000;

std::string join(const std::vector<std::string>& values, const std::string& sep) {
  std::string out;
  for (std::size_t i = 0; i < values.size(); ++i) {
    if (i != 0) {
      out += sep;
    }
    out += values[i];
  }
  return out;
}

std::vector<LadderPreset> ladder_presets() {
  return {{"360p", 360, 400 * kBitrateStep},
          {"480p", 480, 800 * kBitrateStep},
          {"720p", 720, 1'200 * kBitrateStep},
          {"1080p", 1080, 2'400 * kBitrateStep}};
}

std::string stage_input_digest(const std::string& stage,
                               const std::string& upstream) {
  return sha256_hex(stage + ":" + upstream);
}

void emit_manifest(const PipelineConfig& config, const Timeline& timeline,
                   const std::vector<TrackSegments>& per_track,
                   const LadderResult& ladder, const std::string& input_digest,
                   Manifest& manifest) {
  manifest.version = 1;
  manifest.input_digest = input_digest;
  manifest.config_digest = config.config_digest();
  manifest.timebase = timeline.timebase;
  manifest.start_us = timeline.start_us;
  manifest.end_us = timeline.end_us;
  manifest.tracks.clear();
  for (const auto& node : timeline.graph) {
    ManifestTrack track;
    track.id = node.spec.id;
    switch (node.state) {
      case TrackState::kReady: track.state = "ready"; break;
      case TrackState::kBlocked: track.state = "blocked"; break;
      case TrackState::kEmpty: track.state = "empty"; break;
    }
    track.dependency_chain = node.dependency_chain;
    const TrackSegments* segments = nullptr;
    for (const auto& candidate : per_track) {
      if (candidate.track == node.spec.id) {
        segments = &candidate;
      }
    }
    if (segments != nullptr) {
      track.epochs = 1;
      for (const auto& segment : segments->segments) {
        ManifestSegment entry;
        entry.track = segment.track;
        entry.index = segment.index;
        entry.start_us = segment.start_us;
        entry.end_us = segment.end_us;
        entry.start_sequence = segment.start_sequence;
        entry.end_sequence = segment.end_sequence;
        entry.frame_count = segment.frame_count;
        switch (segment.status) {
          case SegmentStatus::kComplete: entry.status = "complete"; break;
          case SegmentStatus::kTailTruncated: entry.status = "tail_truncated"; break;
          case SegmentStatus::kGap: entry.status = "gap"; break;
          case SegmentStatus::kUnseekable: entry.status = "unseekable"; break;
        }
        entry.status_reason = segment.status_reason;
        entry.content_hash = segment.content_hash;
        entry.random_access = segment.random_access;
        for (const auto& rendition : ladder.renditions) {
          if (rendition.segment_index == segment.index) {
            entry.rendition = rendition.name;
            entry.bitrate_bps = rendition.bitrate_bps;
          }
        }
        track.segments.push_back(std::move(entry));
      }
    }
    manifest.tracks.push_back(std::move(track));
  }
  manifest.downgrades = ladder.downgrades;
}

void emit_report(PipelineReport& report, const Timeline& timeline,
                 const std::vector<TrackInput>& inputs,
                 const std::vector<TrackStream>& streams,
                 const std::vector<TrackSegments>& per_track,
                 const LadderResult& ladder, const Manifest& manifest) {
  report.tracks = timeline.graph.size();
  report.empty_tracks = timeline.empty_tracks;
  report.blocked_tracks = timeline.blocked_tracks;
  report.usable_tracks = timeline.tracks.size();
  report.epochs = 0;
  report.segments = 0;
  report.random_access_segments = 0;
  report.unseekable_segments = 0;
  report.aligned_cut_error_frames = 0;
  std::size_t boundary_total = 0;
  std::size_t boundary_hits = 0;
  for (const auto& stream : streams) {
    report.duplicates += stream.stats.duplicates;
    report.reordered += stream.stats.reordered;
    report.lost_burst += stream.stats.lost_marked;
    report.late_dropped += stream.stats.late_dropped;
    report.epochs += stream.epochs.size();
    report.quarantined_frames += stream.stats.unverified_quarantined;
    report.peak_buffer_bytes = std::max<std::uint64_t>(
        report.peak_buffer_bytes, stream.stats.peak_cache_bytes);
  }
  for (const auto& input : inputs) {
    report.checksum_failures += input.checksum_failures;
  }
  for (const auto& segments : per_track) {
    for (const auto& segment : segments.segments) {
      ++report.segments;
      report.segment_hashes.push_back(segment.content_hash);
      if (segment.random_access) {
        ++report.random_access_segments;
      } else {
        ++report.unseekable_segments;
      }
      ++boundary_total;
      if (segment.random_access) {
        ++boundary_hits;
      }
    }
  }
  report.keyframe_hit_rate =
      boundary_total == 0
          ? 0.0
          : static_cast<double>(boundary_hits) /
                static_cast<double>(boundary_total);
  // Shared-cut alignment: every rendition uses the boundary track's
  // segments, so the error is the largest distance from a cut to the
  // nearest source frame of the boundary track.
  const TimelineTrack* boundary = nullptr;
  for (const auto& track : timeline.tracks) {
    if (track.id == ladder.boundary_source) {
      boundary = &track;
    }
  }
  if (boundary != nullptr) {
    const Micros frame_us = static_cast<Micros>(
        boundary->timebase.frame_duration_us());
    Micros worst_us = 0;
    for (const Micros cut : ladder.shared_cuts_us) {
      auto it = std::lower_bound(
          boundary->frames.begin(), boundary->frames.end(), cut,
          [](const FrameRecord& frame, Micros value) {
            return frame.pts_us < value;
          });
      Micros best = std::numeric_limits<Micros>::max();
      if (it != boundary->frames.end()) {
        best = std::min(best, std::llabs(it->pts_us - cut));
      }
      if (it != boundary->frames.begin()) {
        best = std::min(best, std::llabs(std::prev(it)->pts_us - cut));
      }
      if (best != std::numeric_limits<Micros>::max()) {
        worst_us = std::max<Micros>(worst_us, best);
      }
    }
    report.aligned_cut_error_frames =
        frame_us <= 0 ? 0 : static_cast<std::size_t>(worst_us / frame_us);
  }
  report.downgrades = ladder.downgrades.size();
  report.peak_buffer_bytes = std::max<std::uint64_t>(
      report.peak_buffer_bytes, ladder.peak_cache_bytes);
  report.manifest_digest = manifest_digest(manifest);

  // Round-trip check: serialize, parse back with unknown-field preservation
  // and require an identical digest.  Any mismatch is fatal.
  Manifest reparsed;
  ManifestParseResult roundtrip = parse_manifest(serialize(manifest), reparsed);
  report.roundtrip_rate =
      roundtrip.ok && manifest_digest(reparsed) == report.manifest_digest ? 1.0
                                                                        : 0.0;
}

}  // namespace

std::string PipelineConfig::config_digest() const {
  std::ostringstream out;
  out << "seed=" << seed << ";frames=" << frames << ";gop=" << gop
      << ";size=" << width << 'x' << height << ";cams=" << join(cameras, ",")
      << ";segment_ms=" << segment_ms << ";loss_burst=" << loss_burst
      << ";dup=" << duplicate_stride << ";reorder=" << reorder_stride
      << ";ladder=" << join(ladder, ",") << ";bandwidth=";
  for (const auto value : bandwidth) {
    out << value << ',';
  }
  out << ";cache=" << cache_limit_bytes << ";corrupt=" << corrupt
      << ";lose_keyframe=" << lose_keyframe << ";empty=" << empty_track
      << ";empty_cam=" << empty_camera_index;
  return sha256_hex(out.str());
}

PipelineResult run_demo(const PipelineConfig& config) {
  PipelineResult result;
  std::error_code ec;
  std::filesystem::remove_all(config.work_dir, ec);
  std::filesystem::create_directories(config.work_dir / "inputs", ec);
  fixture::GenerateOptions options;
  options.seed = config.seed;
  options.root = config.work_dir;
  options.frames = config.frames;
  options.gop = config.gop;
  options.width = config.width;
  options.height = config.height;
  options.cameras = config.cameras;
  options.segment_ms = static_cast<int>(config.segment_ms);
  options.loss_burst = config.loss_burst;
  options.corrupt = config.corrupt;
  options.corrupt_camera_index = config.cameras.size() > 1 ? 1 : 0;
  options.lose_keyframe = config.lose_keyframe;
  options.empty_track = config.empty_track;
  options.empty_camera_index = config.empty_camera_index;
  options.duplicate_stride = static_cast<int>(config.duplicate_stride);
  options.reorder_stride = static_cast<int>(config.reorder_stride);
  ErrorSink generator_errors;
  fixture::generate(options, generator_errors);
  return run_pipeline(config);
}

PipelineResult run_pipeline(const PipelineConfig& config) {
  PipelineResult result;
  ErrorSink errors;
  const std::string config_digest = config.config_digest();
  CheckpointStore checkpoints(config.work_dir / "state");
  const std::filesystem::path inputs = config.work_dir / "inputs";

  // ---- read -------------------------------------------------------------
  ReadResult read;
  const std::string raw_input_digest =
      fixture::directory_digest(inputs);
  const std::string read_digest =
      stage_input_digest(kStageRead, raw_input_digest + config_digest);
  StageOutcome read_outcome;
  const bool read_reused =
      config.resume &&
      checkpoints.can_reuse(kStageRead, read_digest, read_outcome, errors);
  if (read_reused) {
    ++result.report.resumed_stages;
    result.report.resumed = true;
    std::string parse_error;
    if (!parse_read_cache(read_outcome.record.payload, read, parse_error)) {
      errors.fail("resume", "", 0, "read cache unusable: " + parse_error);
      read = ReadResult{};
    }
  }
  if (read.tracks.empty()) {
    checkpoints.begin(kStageRead, read_digest, errors);
    read = read_frame_inputs(inputs, errors);
    read.input_digest = read.input_digest.empty() ? raw_input_digest
                                                  : read.input_digest;
    if (!checkpoints.complete(kStageRead, read_digest, read.input_digest,
                              serialize_read_cache(read), errors)) {
      errors.fail("checkpoint", kStageRead, 0, "cannot persist read stage");
    }
  } else {
    ++result.report.recomputed_stages;
  }
  if (read.tracks.empty()) {
    result.exit_code = static_cast<int>(ExitCode::kCorrupt);
    result.errors = errors.events();
    return result;
  }

  // ---- reorder ----------------------------------------------------------
  const std::string reorder_digest =
      stage_input_digest(kStageReorder, read.input_digest + config_digest);
  std::vector<TrackStream> streams;
  StageOutcome reorder_outcome;
  const bool reorder_reused =
      config.resume &&
      checkpoints.can_reuse(kStageReorder, reorder_digest, reorder_outcome,
                            errors);
  if (reorder_reused) {
    ++result.report.resumed_stages;
    result.report.resumed = true;
    std::string parse_error;
    if (!parse_reorder_cache(reorder_outcome.record.payload, streams,
                             parse_error)) {
      errors.fail("resume", "", 0,
                  "reorder cache unusable: " + parse_error);
      streams.clear();
    }
  }
  if (streams.empty()) {
    if (!reorder_reused) {
      ++result.report.recomputed_stages;
    }
    checkpoints.begin(kStageReorder, reorder_digest, errors);
    for (const auto& track : read.tracks) {
      streams.push_back(reorder_track(track, errors));
    }
    checkpoints.complete(kStageReorder, reorder_digest, read.input_digest,
                         serialize_reorder_cache(streams), errors);
  }

  // ---- timeline ---------------------------------------------------------
  const std::string timeline_digest =
      stage_input_digest(kStageTimeline, read.input_digest + config_digest);
  checkpoints.begin(kStageTimeline, timeline_digest, errors);
  Timeline timeline = build_timeline(read.tracks, streams, errors);
  if (timeline.graph.empty()) {
    checkpoints.fail(kStageTimeline, timeline_digest,
                     "dependency graph rejected", errors);
    result.exit_code = static_cast<int>(ExitCode::kCorrupt);
    result.errors = errors.events();
    return result;
  }
  const std::string timeline_output =
      sha256_hex(std::to_string(timeline.tracks.size()) + ":" +
                 std::to_string(timeline.empty_tracks.size()) + ":" +
                 std::to_string(timeline.blocked_tracks.size()));
  checkpoints.complete(kStageTimeline, timeline_digest, timeline_output,
                       {}, errors);

  // ---- segment ----------------------------------------------------------
  SegmentOptions segment_options;
  segment_options.target_duration_us = config.segment_ms * 1000;
  segment_options.max_boundary_error_us = static_cast<Micros>(
      timeline.timebase.frame_duration_us());
  std::vector<TrackSegments> per_track;
  std::size_t segment_index = 0;
  for (const auto& track : timeline.tracks) {
    TrackSegments segments = segment_track(track, segment_options, errors);
    if (config.interrupt_after_segment &&
        segment_index + segments.segments.size() > config.interrupt_segment) {
      // Persist what has been produced so far and stop with a resumable
      // state; nothing downstream is emitted for a partial segment list.
      per_track.push_back(std::move(segments));
      result.report.interrupted = true;
      break;
    }
    segment_index += segments.segments.size();
    per_track.push_back(std::move(segments));
  }
  if (result.report.interrupted) {
    // Mark the incomplete stage so a resume recomputes it rather than
    // trusting a partial result.
    checkpoints.begin(kStageSegment, timeline_digest, errors);
    result.report.input_digest = read.input_digest;
    result.report.config_digest = config_digest;
    result.report.stage_order = {"read", "reorder", "timeline"};
    result.exit_code = 0;
    result.errors = errors.events();
    return result;
  }

  std::size_t segment_total = 0;
  std::size_t segment_unseekable = 0;
  std::string segment_hashes;
  for (const auto& segments : per_track) {
    for (const auto& segment : segments.segments) {
      segment_hashes += segment.content_hash;
      ++segment_total;
      if (!segment.random_access) {
        ++segment_unseekable;
      }
    }
  }
  const std::string segment_output = sha256_hex(segment_hashes);
  const std::string segment_digest =
      stage_input_digest(kStageSegment, timeline_output + config_digest);
  checkpoints.complete(kStageSegment, segment_digest, segment_output,
                       std::to_string(segment_total) + ":" +
                           std::to_string(segment_unseekable),
                       errors);

  // ---- ladder -----------------------------------------------------------
  LadderOptions ladder_options;
  const auto presets = ladder_presets();
  for (const auto& name : config.ladder) {
    for (const auto& preset : presets) {
      if (preset.name == name) {
        ladder_options.representations.push_back(
            Representation{preset.name, preset.height, preset.bitrate_bps});
      }
    }
  }
  if (ladder_options.representations.empty()) {
    errors.fail("ladder", "", 0,
                "no known rendition in --ladder " + join(config.ladder, ","));
    result.exit_code = static_cast<int>(ExitCode::kUsage);
    result.errors = errors.events();
    return result;
  }
  ladder_options.cache_limit_bytes = config.cache_limit_bytes;
  ladder_options.window_samples = 3;

  // Bandwidth samples: one per segment boundary for each configured value,
  // expressed in kbps and spread over the timeline span.
  std::vector<BandwidthSample> samples;
  const TrackSegments* boundary = nullptr;
  for (const auto& track : per_track) {
    if (!track.segments.empty()) {
      boundary = &track;
      break;
    }
  }
  if (boundary != nullptr) {
    const auto& cuts = boundary->segments;
    for (std::size_t i = 0; i < cuts.size(); ++i) {
      const auto kbps = config.bandwidth[i % config.bandwidth.size()];
      samples.push_back(
          BandwidthSample{cuts[i].start_us, kbps * kBitrateStep});
    }
  }
  LadderResult ladder;
  if (boundary != nullptr) {
    ladder = align_ladder(*boundary, samples, ladder_options, errors);
  }
  std::string ladder_plan;
  for (const auto& rendition : ladder.renditions) {
    ladder_plan +=
        std::to_string(rendition.segment_index) + ':' + rendition.name + ';';
  }
  const std::string ladder_digest =
      stage_input_digest(kStageLadder, segment_output + config_digest);
  checkpoints.complete(kStageLadder, ladder_digest,
                       sha256_hex(ladder_plan), ladder_plan, errors);

  // ---- manifest ---------------------------------------------------------
  Manifest manifest;
  emit_manifest(config, timeline, per_track, ladder, read.input_digest,
                manifest);
  PipelineReport& report = result.report;
  emit_report(report, timeline, read.tracks, streams, per_track, ladder,
              manifest);
  report.input_digest = read.input_digest;
  report.config_digest = config_digest;

  const std::filesystem::path manifest_path =
      config.work_dir / "out" / "manifest.json";
  std::error_code ec;
  std::filesystem::create_directories(manifest_path.parent_path(), ec);
  std::string store_error;
  const bool stored = store_manifest(manifest_path, manifest, store_error);
  result.manifest_path = manifest_path;
  if (!stored) {
    errors.fail("manifest", "", 0, store_error);
  }
  // Round-trip verification of what was actually written.
  Manifest reloaded;
  ManifestParseResult reload = load_manifest(manifest_path, reloaded);
  if (!stored || !reload.ok ||
      manifest_digest(reloaded) != report.manifest_digest) {
    errors.fail("manifest", "", 0,
                "manifest failed round-trip verification; not published");
    report.roundtrip_rate = 0.0;
  }
  const std::string manifest_stage_digest =
      stage_input_digest(kStageManifest,
                         report.manifest_digest + config_digest);
  checkpoints.begin(kStageManifest, manifest_stage_digest, errors);
  if (!errors.has_errors()) {
    checkpoints.complete(kStageManifest, manifest_stage_digest,
                         report.manifest_digest, std::string(), errors);
  } else {
    checkpoints.fail(kStageManifest, manifest_stage_digest,
                     "verification failed", errors);
  }
  report.stage_order.clear();
  for (const auto& stage : checkpoints.completed_order()) {
    report.stage_order.push_back(stage);
  }
  result.manifest = std::move(manifest);
  result.errors = errors.events();
  result.exit_code = errors.exit_code();
  return result;
}

}  // namespace relay

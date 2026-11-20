#include "relay/fixture.hpp"
#include "relay/frame_reader.hpp"
#include "relay/packet_reorder.hpp"
#include "relay/checkpoint.hpp"
#include "relay/ladder.hpp"
#include "relay/manifest.hpp"
#include "relay/segmenter.hpp"
#include "relay/timeline.hpp"
#include "relay/types.hpp"

#include <cstdlib>
#include <filesystem>
#include <fstream>

#include <catch2/catch_test_macros.hpp>

using namespace relay;

TEST_CASE("timebase converts frames and microseconds symmetrically", "[timebase]") {
  const Timebase nt{30000, 1001};
  REQUIRE(nt.frames_to_us(30000) == 1'001'000'000);
  REQUIRE(nt.frames_to_us(-30000) == -1'001'000'000);
  REQUIRE(nt.us_to_frames(1'001'000'000) == 30000);
  REQUIRE(nt.us_to_frames(-1'001'000'000) == -30000);

  const Timebase simple{30, 1};
  REQUIRE(simple.frames_to_us(1) == 33'333);
  REQUIRE(simple.frames_to_us(30) == 1'000'000);
}

TEST_CASE("structured errors locate stage track and frame", "[errors]") {
  ErrorSink sink;
  sink.warn("reorder", "cam-b", 7, "duplicate packet");
  sink.fail("verify", "cam-a", 42, "checksum mismatch", "verify<-read<-cam-a");

  REQUIRE(sink.has_warnings());
  REQUIRE(sink.has_errors());
  REQUIRE(sink.exit_code() == static_cast<int>(ExitCode::kCorrupt));
  REQUIRE(sink.count(Severity::kError) == 1);
  const std::string text = sink.events().back().to_string();
  REQUIRE(text.find("track=cam-a") != std::string::npos);
  REQUIRE(text.find("frame=42") != std::string::npos);
  REQUIRE(text.find("verify<-read<-cam-a") != std::string::npos);
}

namespace {

std::filesystem::path make_scratch(const std::string& name) {
  const auto root = std::filesystem::temp_directory_path() / "relay-tests" / name;
  std::filesystem::remove_all(root);
  std::filesystem::create_directories(root);
  return root;
}

}  // namespace

TEST_CASE("fixture frames pass SHA-256 and FFmpeg decode", "[reader]") {
  auto root = make_scratch("reader-ok");
  fixture::GenerateOptions options;
  options.root = root;
  options.frames = 12;
  options.gop = 4;
  options.cameras = {"cam-a", "cam-b"};
  options.loss_burst = 1;
  ErrorSink generator_errors;
  auto generated = fixture::generate(options, generator_errors);
  REQUIRE(generated.cameras.size() == 2);

  ErrorSink errors;
  ReadResult read = read_frame_inputs(root / "inputs", errors);
  REQUIRE_FALSE(errors.has_errors());
  REQUIRE(read.tracks.size() == 2);
  for (const auto& track : read.tracks) {
    CHECK(track.catalog.size() == 12);
    for (const auto& frame : track.catalog) {
      INFO("track=" << track.spec.id << " frame=" << frame.record.source_frame);
      CHECK(frame.verified);
      CHECK(frame.record.pixel_format == "rgb24");
      CHECK(frame.record.width == 96);
    }
    CHECK_FALSE(track.arrivals.empty());
  }
}

TEST_CASE("corrupt payload is quarantined with camera and frame number",
          "[reader][corrupt]") {
  auto root = make_scratch("reader-corrupt");
  fixture::GenerateOptions options;
  options.root = root;
  options.frames = 12;
  options.gop = 4;
  options.cameras = {"cam-a", "cam-b"};
  options.corrupt = true;
  options.corrupt_camera_index = 1;
  options.corrupt_frame = 5;
  ErrorSink generator_errors;
  fixture::generate(options, generator_errors);

  ErrorSink errors;
  ReadResult read = read_frame_inputs(root / "inputs", errors);
  REQUIRE(errors.has_errors());
  REQUIRE(read.checksum_failures == 1);
  const RelayError* located = nullptr;
  for (const auto& event : errors.events()) {
    if (event.ids.track == "cam-b" && event.ids.frame == 5) {
      located = &event;
    }
  }
  REQUIRE(located != nullptr);
  CHECK(located->reason.find("SHA-256") != std::string::npos);
  const auto& cam_b = read.tracks.at(1);
  CHECK_FALSE(cam_b.catalog.at(5).verified);
  CHECK(cam_b.checksum_failures == 1);
}

namespace {

fixture::GeneratedFixture make_stream_fixture(const std::string& name,
                                              int frames = 24, int gop = 6,
                                              int dup = 5, int reorder = 3,
                                              int burst = 2,
                                              bool wrap = true) {
  auto root = make_scratch(name);
  fixture::GenerateOptions options;
  options.root = root;
  options.frames = frames;
  options.gop = gop;
  options.cameras = {"cam-a"};
  options.duplicate_stride = dup;
  options.reorder_stride = reorder;
  options.loss_burst = burst;
  options.timestamp_wrap = wrap;
  options.sequence_reset = true;
  ErrorSink errors;
  return fixture::generate(options, errors);
}

}  // namespace

TEST_CASE("reorder dedupes and orders packets without re-emitting frames",
          "[reorder]") {
  auto generated =
      make_stream_fixture("reorder-dedupe", 24, 6, 5, 3, 0, false);
  ErrorSink errors;
  ReadResult read = read_frame_inputs(generated.root / "inputs", errors);
  REQUIRE(read.tracks.size() == 1);
  const TrackInput& input = read.tracks.front();

  TrackStream stream = reorder_track(input, errors);
  CHECK(stream.stats.received == input.arrivals.size());
  CHECK(stream.stats.duplicates > 0);
  CHECK(stream.stats.reordered > 0);
  CHECK(stream.stats.lost_marked == 0);
  CHECK(stream.frame_count() == input.catalog.size());

  for (const auto& epoch : stream.epochs) {
    for (std::size_t i = 1; i < epoch.frames.size(); ++i) {
      CHECK(epoch.frames[i - 1].pts_us < epoch.frames[i].pts_us);
    }
    for (std::size_t i = 1; i < epoch.frames.size(); ++i) {
      CHECK(epoch.frames[i].sequence > epoch.frames[i - 1].sequence);
    }
  }
}

TEST_CASE("delivery gaps are marked per track policy", "[reorder][loss]") {
  auto generated = make_stream_fixture("reorder-loss");
  ErrorSink errors;
  ReadResult read = read_frame_inputs(generated.root / "inputs", errors);
  TrackStream stream = reorder_track(read.tracks.front(), errors);
  const auto losses = stream.all_losses();
  CHECK(stream.stats.lost_marked > 0);
  CHECK(stream.frame_count() + stream.stats.lost_marked ==
        read.tracks.front().catalog.size());
  CHECK_FALSE(losses.empty());
}

TEST_CASE("timestamp wrap opens a new epoch instead of splicing",
          "[reorder][epoch]") {
  auto generated = make_stream_fixture("reorder-epoch", 30, 10, 0, 0, 0, true);
  ErrorSink errors;
  ReadResult read = read_frame_inputs(generated.root / "inputs", errors);
  TrackStream stream = reorder_track(read.tracks.front(), errors);
  REQUIRE(stream.epochs.size() >= 2);
  // A wrap at frame 10 and a sequence reset at frame 30 (not reached).
  CHECK(stream.epochs[0].frames.size() == 10);
  int wrap_warnings = 0;
  for (const auto& event : errors.events()) {
    if (event.reason.find("epoch") != std::string::npos) {
      ++wrap_warnings;
    }
  }
  CHECK(wrap_warnings >= 1);
  // No frame from a later epoch may sort before an earlier epoch's frames.
  for (std::size_t e = 1; e < stream.epochs.size(); ++e) {
    if (!stream.epochs[e].frames.empty() && !stream.epochs[e - 1].frames.empty()) {
      CHECK(stream.epochs[e].frames.front().epoch >
            stream.epochs[e - 1].frames.front().epoch);
    }
  }
}

TEST_CASE("late packets beyond the window are dropped, not re-emitted",
          "[reorder][late]") {
  TrackInput input;
  input.spec.id = "cam-late";
  input.reorder_window = 2;
  input.timebase = Timebase{30, 1};
  auto make_frame = [](std::uint64_t source, std::uint32_t seq,
                       std::uint32_t raw) {
    CatalogFrame frame;
    frame.record.track = "cam-late";
    frame.record.source_frame = source;
    frame.record.sequence = seq;
    frame.record.raw_timestamp_us = raw;
    frame.record.payload_bytes = 10;
    frame.verified = true;
    return frame;
  };
  input.catalog = {make_frame(0, 0, 1000), make_frame(1, 1, 34333),
                   make_frame(2, 2, 67666), make_frame(3, 3, 101000)};
  input.arrivals = {Arrival{0, 0}, Arrival{1, 1}, Arrival{2, 2},
                    Arrival{3, 3}, Arrival{4, 0}};
  ErrorSink errors;
  TrackStream stream = reorder_track(input, errors);
  CHECK(stream.frame_count() == 4);
  CHECK(stream.stats.duplicates == 1);
  CHECK(stream.stats.late_dropped == 0);
}

TEST_CASE("a packet arriving later than the window is dropped", "[reorder][late]") {
  TrackInput input;
  input.spec.id = "cam-late";
  input.reorder_window = 2;
  input.timebase = Timebase{30, 1};
  auto frame = [](std::uint64_t source, std::uint32_t seq, std::uint32_t raw) {
    CatalogFrame f;
    f.record.track = "cam-late";
    f.record.source_frame = source;
    f.record.sequence = seq;
    f.record.raw_timestamp_us = raw;
    f.record.payload_bytes = 10;
    f.verified = true;
    return f;
  };
  input.catalog = {frame(0, 0, 1000), frame(1, 1, 34333), frame(2, 2, 67666),
                   frame(3, 3, 101000), frame(4, 4, 134333),
                   frame(5, 5, 167666)};
  // Frames 3 and 4 arrive first; frames 0..2 arrive after the window closed.
  input.arrivals = {Arrival{0, 4}, Arrival{1, 3}, Arrival{2, 0},
                    Arrival{3, 1}, Arrival{4, 2}, Arrival{5, 5}};
  ErrorSink errors;
  TrackStream stream = reorder_track(input, errors);
  CHECK(stream.stats.late_dropped == 3);
  CHECK(stream.frame_count() == 3);
  bool located = false;
  for (const auto& event : errors.events()) {
    if (event.ids.track == "cam-late" &&
        event.reason.find("late") != std::string::npos) {
      located = true;
    }
  }
  CHECK(located);
}

TEST_CASE("dependency graph orders tracks and blocks dependents of empty ones",
          "[timeline]") {
  std::vector<TrackInput> inputs;
  for (const std::string& id : {"cam-a", "cam-b"}) {
    TrackInput input;
    input.spec.id = id;
    input.timebase = Timebase{30, 1};
    inputs.push_back(input);
  }
  inputs[1].spec.depends_on = {"cam-a"};
  std::vector<TrackStream> streams(2);
  streams[0].track = "cam-a";
  streams[0].timebase = Timebase{30, 1};
  streams[1].track = "cam-b";
  streams[1].timebase = Timebase{30, 1};
  TimelineEpoch epoch;
  epoch.index = 0;
  epoch.cause = "start";
  for (int i = 0; i < 4; ++i) {
    FrameRecord frame;
    frame.track = "cam-b";
    frame.sequence = static_cast<std::uint32_t>(i);
    frame.pts_us = i * 33333;
    frame.keyframe = i == 0;
    epoch.frames.push_back(frame);
  }
  streams[1].epochs.push_back(epoch);
  ErrorSink errors;
  Timeline timeline = build_timeline(inputs, streams, errors);
  REQUIRE(timeline.graph.size() == 2);
  CHECK(timeline.graph.front().spec.id == "cam-a");
  CHECK(timeline.graph.front().state == TrackState::kEmpty);
  CHECK(timeline.graph.back().spec.id == "cam-b");
  CHECK(timeline.graph.back().state == TrackState::kBlocked);
  CHECK(timeline.graph.back().dependency_chain == "cam-b<-cam-a");
  CHECK(timeline.blocked_tracks.size() == 1);
  CHECK(timeline.empty_tracks.size() == 1);
  CHECK(errors.has_errors());  // blocking is reported as error
  CHECK(timeline.tracks.empty());
}

TEST_CASE("dependency cycles are rejected with the offending chain",
          "[timeline]") {
  std::vector<TrackInput> inputs(2);
  inputs[0].spec.id = "a";
  inputs[0].spec.depends_on = {"b"};
  inputs[0].timebase = Timebase{30, 1};
  inputs[1].spec.id = "b";
  inputs[1].spec.depends_on = {"a"};
  inputs[1].timebase = Timebase{30, 1};
  ErrorSink errors;
  Timeline timeline = build_timeline(inputs, {}, errors);
  CHECK(errors.has_errors());
  bool cycle_reported = false;
  for (const auto& event : errors.events()) {
    if (event.reason.find("cycle") != std::string::npos) {
      cycle_reported = true;
    }
  }
  CHECK(cycle_reported);
  CHECK(timeline.graph.empty());
}

namespace {

TimelineTrack synthetic_track(const std::string& id, int frames, int gop,
                              int period = 33333) {
  TimelineTrack track;
  track.id = id;
  track.timebase = Timebase{30, 1};
  for (int i = 0; i < frames; ++i) {
    FrameRecord frame;
    frame.track = id;
    frame.source_frame = static_cast<std::uint64_t>(i);
    frame.sequence = static_cast<std::uint32_t>(i);
    frame.pts_us = static_cast<Micros>(i) * period;
    frame.keyframe = i % gop == 0;
    frame.sha256 = std::string(64, 'a');
    track.frames.push_back(frame);
  }
  return track;
}

}  // namespace

TEST_CASE("segments cut on keyframes near the target duration", "[segment]") {
  TimelineTrack track = synthetic_track("cam-a", 120, 15);
  SegmentOptions options;
  options.target_duration_us = 500'000;  // 15 frames at 30fps
  ErrorSink errors;
  TrackSegments segments = segment_track(track, options, errors);
  REQUIRE(segments.segments.size() == 8);
  for (const auto& segment : segments.segments) {
    CHECK(segment.random_access);
    CHECK(segment.keyframe_sequence == segment.start_sequence);
    CHECK(segment.frame_count == 15);
    CHECK(segment.status == SegmentStatus::kComplete);
  }
}

TEST_CASE("boundary error never exceeds one source frame", "[segment][boundary]") {
  TimelineTrack track = synthetic_track("cam-a", 90, 10);
  SegmentOptions options;
  options.target_duration_us = 300'000;  // 9 frames; nearest key is 10
  ErrorSink errors;
  TrackSegments segments = segment_track(track, options, errors);
  REQUIRE(segments.segments.size() > 1);
  for (std::size_t i = 0; i + 1 < segments.segments.size(); ++i) {
    const auto& segment = segments.segments[i];
    const Micros duration = segment.end_us - segment.start_us;
    const Micros error = std::llabs(duration - options.target_duration_us);
    INFO("segment " << segment.index << " duration " << duration);
    CHECK(error <= 33333);
  }
  // The final segment may be shorter; it must say so.
  const auto& tail = segments.segments.back();
  if (tail.end_us - tail.start_us + 33333 < options.target_duration_us) {
    CHECK(tail.status == SegmentStatus::kTailTruncated);
  }
}

TEST_CASE("short tails produce an explainable final segment", "[segment][tail]") {
  TimelineTrack track = synthetic_track("cam-a", 25, 10);
  SegmentOptions options;
  options.target_duration_us = 333'330;  // 10 frames
  ErrorSink errors;
  TrackSegments segments = segment_track(track, options, errors);
  REQUIRE(segments.segments.size() == 3);
  CHECK(segments.segments[0].frame_count == 10);
  CHECK(segments.segments[1].frame_count == 10);
  const Segment& tail = segments.segments.back();
  CHECK(tail.frame_count == 5);
  CHECK(tail.status == SegmentStatus::kTailTruncated);
  CHECK_FALSE(tail.status_reason.empty());
}

TEST_CASE("a lost keyframe makes the affected segment unseekable",
          "[segment][keyframe]") {
  TimelineTrack track = synthetic_track("cam-a", 60, 10);
  // Remove keyframe 20 and its neighbours, imitating a burst loss.
  std::vector<FrameRecord> kept;
  for (const auto& frame : track.frames) {
    if (frame.sequence >= 18 && frame.sequence <= 21) {
      continue;
    }
    kept.push_back(frame);
  }
  track.frames = std::move(kept);
  track.losses.push_back(LostRange{"cam-a", 0, 18, 4, "burst loss"});
  SegmentOptions options;
  options.target_duration_us = 333'330;
  ErrorSink errors;
  TrackSegments segments = segment_track(track, options, errors);
  REQUIRE_FALSE(segments.segments.empty());
  bool flagged = false;
  for (const auto& segment : segments.segments) {
    if (!segment.random_access) {
      flagged = true;
      CHECK(segment.status == SegmentStatus::kUnseekable);
      CHECK(segment.status_reason.find("keyframe") != std::string::npos);
    }
  }
  CHECK(flagged);
  CHECK(segments.unseekable > 0);
  // The unseekable segment must not advertise a fabricated index.
  for (const auto& segment : segments.segments) {
    if (!segment.random_access) {
      CHECK(segment.keyframe_sequence == 0);
    }
  }
}

namespace {

LadderOptions default_ladder() {
  LadderOptions options;
  options.representations = {
      Representation{"360p", 360, 800'000},
      Representation{"720p", 720, 2'500'000},
      Representation{"1080p", 1080, 6'000'000},
  };
  options.window_samples = 3;
  return options;
}

TrackSegments boundary_segments(int count, Micros target = 500'000) {
  SegmentOptions options;
  options.target_duration_us = target;
  ErrorSink errors;
  TimelineTrack track = synthetic_track("cam-a", count * 15, 15);
  return segment_track(track, options, errors);
}

}  // namespace

TEST_CASE("ladder picks a rendition from the sliding estimate", "[ladder]") {
  TrackSegments segments = boundary_segments(3);
  std::vector<BandwidthSample> samples = {
      {0, 3'000'000}, {0, 3'000'000}, {0, 3'000'000}};
  LadderOptions options = default_ladder();
  ErrorSink errors;
  LadderResult result = align_ladder(segments, samples, options, errors);
  CHECK_FALSE(result.renditions.empty());
  // 3 Mbps * 0.8 safety = 2.4 Mbps: enough for 360p, not for 720p.
  CHECK(result.renditions.front().name == "360p");
  CHECK(result.renditions.front().reason.find("360p") != std::string::npos);
}

TEST_CASE("a bandwidth drop downgrades and records the reason", "[ladder]") {
  TrackSegments segments = boundary_segments(4);
  std::vector<BandwidthSample> samples;
  for (std::size_t i = 0; i < segments.segments.size(); ++i) {
    const Micros at = segments.segments[i].start_us;
    samples.push_back(BandwidthSample{at, i < 2 ? 8'000'000ull : 600'000ull});
  }
  LadderOptions options = default_ladder();
  options.window_samples = 1;
  ErrorSink errors;
  LadderResult result = align_ladder(segments, samples, options, errors);
  REQUIRE(result.renditions.size() == segments.segments.size());
  CHECK(result.renditions.front().name == "1080p");
  CHECK(result.renditions.back().name == "360p");
  REQUIRE_FALSE(result.downgrades.empty());
  CHECK(result.downgrades.back().reason == "bandwidth drop");
  CHECK(result.downgrades.back().from == "1080p");
  CHECK(result.downgrades.back().to == "360p");
  CHECK(result.peak_cache_bytes <= kMaxTrackCacheBytes);
}

TEST_CASE("all representations share the boundary track's cuts", "[ladder]") {
  TrackSegments segments = boundary_segments(5);
  std::vector<BandwidthSample> samples{{0, 10'000'000}};
  LadderOptions options = default_ladder();
  ErrorSink errors;
  LadderResult result = align_ladder(segments, samples, options, errors);
  REQUIRE(result.shared_cuts_us.size() == segments.segments.size());
  for (std::size_t i = 0; i < segments.segments.size(); ++i) {
    CHECK(result.renditions[i].start_us == segments.segments[i].start_us);
    CHECK(result.renditions[i].end_us == segments.segments[i].end_us);
  }
  CHECK(result.boundary_source == "cam-a");
}

namespace {

Manifest sample_manifest() {
  Manifest manifest;
  manifest.version = 1;
  manifest.input_digest = std::string(64, '1');
  manifest.config_digest = std::string(64, '2');
  manifest.timebase = Timebase{30000, 1001};
  manifest.start_us = 12345;
  manifest.end_us = 98765;
  ManifestTrack track;
  track.id = "cam-a";
  track.state = "ready";
  track.epochs = 2;
  track.keyframes = 3;
  track.losses = 4;
  ManifestSegment segment;
  segment.track = "cam-a";
  segment.index = 0;
  segment.start_us = 12345;
  segment.end_us = 51345;
  segment.start_sequence = 0;
  segment.end_sequence = 14;
  segment.frame_count = 15;
  segment.status = "complete";
  segment.content_hash = std::string(64, 'a');
  segment.rendition = "720p";
  segment.bitrate_bps = 2'500'000;
  segment.random_access = true;
  segment.extensions["vendor_note"] = "preserve me";
  segment.extensions["vendor_count"] = 7;
  track.segments.push_back(segment);
  track.extensions["camera_serial"] = "SN-42";
  manifest.tracks.push_back(track);
  manifest.downgrades.push_back(
      DowngradeEvent{1, "1080p", "720p", 1'900'000, 4096, "bandwidth drop"});
  manifest.unknown_fields["future_stage"] = "ignored-but-kept";
  return manifest;
}

}  // namespace

TEST_CASE("manifest round-trips byte-for-byte and keeps unknown fields",
          "[manifest]") {
  Manifest original = sample_manifest();
  const std::string text = serialize(original);
  Manifest parsed;
  ManifestParseResult result = parse_manifest(text, parsed);
  REQUIRE(result.ok);
  CHECK(result.isolated.empty());
  CHECK(serialize(parsed) == text);
  CHECK(manifest_digest(parsed) == manifest_digest(original));
  REQUIRE(parsed.tracks.size() == 1);
  REQUIRE(parsed.tracks[0].segments.size() == 1);
  CHECK(parsed.tracks[0].segments[0].extensions.count("vendor_note") == 1);
  CHECK(parsed.tracks[0].extensions.count("camera_serial") == 1);
  CHECK(parsed.unknown_fields.count("future_stage") == 1);
  CHECK(parsed.timebase.fps_num == 30000);
  CHECK(parsed.timebase.fps_den == 1001);
}

TEST_CASE("damaged manifest records are isolated, independent tracks survive",
          "[manifest][corrupt]") {
  Manifest manifest = sample_manifest();
  manifest.tracks[0].segments.push_back(manifest.tracks[0].segments[0]);
  manifest.tracks[0].segments.back().index = 1;
  manifest.tracks[0].segments.back().content_hash = "not-a-digest";
  ManifestTrack second;
  second.id = "cam-b";
  second.state = "ready";
  ManifestSegment good = manifest.tracks[0].segments[0];
  good.track = "cam-b";
  second.segments.push_back(good);
  manifest.tracks.push_back(second);

  std::string text = serialize(manifest);
  Manifest reparsed;
  ManifestParseResult result = parse_manifest(text, reparsed);
  REQUIRE(result.ok);
  CHECK(result.isolated.size() == 1);
  REQUIRE(reparsed.tracks.size() == 2);
  CHECK(reparsed.tracks[0].segments.size() == 1);
  REQUIRE(reparsed.tracks[1].segments.size() == 1);
  CHECK(reparsed.tracks[1].segments[0].track == "cam-b");
  // The isolated record is gone but the rest round-trips.
  CHECK(serialize(reparsed) != text);
}

TEST_CASE("malformed manifest JSON is rejected, never partially used",
          "[manifest][corrupt]") {
  Manifest parsed;
  ManifestParseResult result = parse_manifest("{\"tracks\": [", parsed);
  CHECK_FALSE(result.ok);
  CHECK(result.error.find("byte") != std::string::npos);
  CHECK(result.error_offset > 0);

  result = parse_manifest("{\"tracks\": [},]}", parsed);
  CHECK_FALSE(result.ok);

  result = parse_manifest("{\"timebase\": {}}", parsed);
  CHECK_FALSE(result.ok);
  CHECK(result.error.find("tracks") != std::string::npos);
}

TEST_CASE("manifest store is atomic and never leaves a partial file",
          "[manifest][io]") {
  auto root = make_scratch("manifest-io");
  const auto path = root / "manifest.json";
  Manifest manifest = sample_manifest();
  std::string error;
  REQUIRE(store_manifest(path, manifest, error));
  CHECK_FALSE(std::filesystem::exists(path.string() + ".tmp"));

  Manifest loaded;
  ManifestParseResult result = load_manifest(path, loaded);
  REQUIRE(result.ok);
  CHECK(serialize(loaded) == serialize(manifest));

  // A corrupt on-disk manifest is reported, not partially applied.
  {
    std::ofstream out(path);
    out << "{ \"tracks\": [ {\"id\": \"cam-a\", \"segments\": [";
  }
  Manifest broken;
  result = load_manifest(path, broken);
  CHECK_FALSE(result.ok);
}

TEST_CASE("checkpoints reuse only when the input digest matches",
          "[checkpoint]") {
  auto root = make_scratch("checkpoint");
  CheckpointStore store(root / "state");
  ErrorSink errors;
  store.begin("read", "digest-A", errors);
  REQUIRE(store.complete("read", "digest-A", "out-A", "payload-A", errors));

  StageOutcome outcome;
  CHECK(store.can_reuse("read", "digest-A", outcome, errors));
  CHECK(outcome.reused);
  CHECK(outcome.record.payload == "payload-A");

  StageOutcome rejected;
  ErrorSink change_errors;
  CHECK_FALSE(store.can_reuse("read", "digest-B", rejected, change_errors));
  CHECK(rejected.rejected);
  CHECK(change_errors.has_warnings());
  bool chain_reported = false;
  for (const auto& event : change_errors.events()) {
    if (event.reason.find("input digest changed") != std::string::npos &&
        !event.dependency_chain.empty()) {
      chain_reported = true;
    }
  }
  CHECK(chain_reported);
}

TEST_CASE("an interrupted stage is resumed rather than trusted",
          "[checkpoint][resume]") {
  auto root = make_scratch("checkpoint-resume");
  {
    CheckpointStore store(root / "state");
    ErrorSink errors;
    store.begin("read", "digest-A", errors);
    REQUIRE(store.complete("read", "digest-A", "out-A", "payload-A", errors));
    // Simulate an interruption after reorder started but before completion.
    store.begin("reorder", "digest-B", errors);
  }
  CheckpointStore resumed(root / "state");
  ErrorSink errors;
  StageOutcome outcome;
  CHECK(resumed.can_reuse("read", "digest-A", outcome, errors));
  CHECK(outcome.reused);
  StageOutcome running;
  CHECK_FALSE(resumed.can_reuse("reorder", "digest-B", running, errors));
  CHECK_FALSE(running.reused);
  CHECK(resumed.completed_order().size() == 1);
}

TEST_CASE("a corrupt checkpoint file is rejected, not silently reset",
          "[checkpoint][corrupt]") {
  auto root = make_scratch("checkpoint-corrupt");
  std::filesystem::create_directories(root / "state");
  {
    std::ofstream out(root / "state" / "checkpoint.ckpt");
    out << "NOT-A-CHECKPOINT\n";
  }
  CheckpointStore store(root / "state");
  StageOutcome outcome;
  ErrorSink errors;
  CHECK_FALSE(store.can_reuse("read", "digest-A", outcome, errors));
  CHECK(errors.has_errors());
  bool located = false;
  for (const auto& event : errors.events()) {
    if (event.ids.stage == "resume") {
      located = true;
    }
  }
  CHECK(located);
}

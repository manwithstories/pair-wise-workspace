#include "relay/fixture.hpp"
#include "relay/frame_reader.hpp"
#include "relay/packet_reorder.hpp"
#include "relay/types.hpp"

#include <filesystem>

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

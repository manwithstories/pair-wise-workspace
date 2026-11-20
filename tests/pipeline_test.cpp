#include "relay/fixture.hpp"
#include "relay/frame_reader.hpp"
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

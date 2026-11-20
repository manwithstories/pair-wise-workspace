#include "media/index_writer.hpp"

#include <catch2/catch_test_macros.hpp>

#include <filesystem>
#include <string>
#include <vector>

#include "fixture_factory.hpp"
#include "media/frame_reader.hpp"
#include "media/generate.hpp"
#include "media/index.hpp"
#include "media/timeline.hpp"

extern "C" {
#include <libavformat/avformat.h>
}

using namespace mediaqc;
using mediaqc::testing::TempDir;

namespace {

struct Source {
  std::vector<SegmentWriter::TrackSpec> specs;
  std::vector<std::vector<FrameMetadata>> metadata;
  std::vector<std::vector<FrameReader::Packet>> packets;
};

Source load_source(const std::string& dir) {
  DeliveryLoadResult delivery = load_delivery(dir);
  REQUIRE(delivery.ok);
  Source source;
  for (const DeliveryTrack& track : delivery.delivery.tracks) {
    SegmentWriter::TrackSpec spec;
    spec.desc = track.desc;
    source.specs.push_back(spec);

    std::string meta_path;
    std::string media_path;
    Error error;
    REQUIRE(resolve_delivery_path(dir, track.metadata_file, meta_path, error));
    REQUIRE(resolve_delivery_path(dir, track.media_file, media_path, error));

    std::vector<FrameMetadata> frames;
    REQUIRE(load_frame_metadata(meta_path, track.desc, frames, error));
    source.metadata.push_back(std::move(frames));

    FrameReader reader;
    REQUIRE(reader.open(media_path, track.desc, FrameReader::Options{}, error));
    FrameReader::Packet packet;
    bool eof = false;
    std::vector<FrameReader::Packet> collected;
    while (reader.next(packet, eof, error)) {
      collected.push_back(std::move(packet));
    }
    source.packets.push_back(std::move(collected));
  }
  return source;
}

bool write_all(SegmentWriter& writer, const Source& source, MediaIndex& index,
               Error& error) {
  for (std::size_t t = 0; t < source.specs.size(); ++t) {
    TrackTimeline timeline(
        TimelineConfig{source.specs[t].desc.rate, source.specs[t].desc.clock});
    for (std::size_t i = 0; i < source.packets[t].size(); ++i) {
      const FrameMetadata& meta = source.metadata[t][i];
      auto mapping = timeline.map(meta.has_capture, meta.capture_ticks);
      SegmentWriter::FrameInput input;
      input.time_ns = mapping.ns;
      input.frame = meta.seq;
      input.frame_index = i;
      input.keyframe = source.packets[t][i].keyframe;
      input.sha256 = meta.sha;
      input.payload = source.packets[t][i].data;
      IndexEntry ignored;
      if (!writer.write(t, input, ignored, error)) return false;
    }
  }
  return writer.finish(index, error);
}

}  // namespace

TEST_CASE("segment writer emits one entry per source frame with real offsets",
          "[index]") {
  TempDir dir;
  REQUIRE(generate_delivery(dir.path().string(),
                            GenerateOptions{Scenario::kNormal, 300, 13307}, true)
              .ok);
  Source source = load_source(dir.path().string());
  const std::string out = (dir.path() / "out").string();

  SegmentWriter writer;
  Error error;
  REQUIRE(writer.open(out, source.specs, false, error));
  MediaIndex index;
  REQUIRE(write_all(writer, source, index, error));

  std::size_t total_frames = 0;
  for (const auto& frames : source.metadata) total_frames += frames.size();
  CHECK(index.entries.size() == total_frames);
  CHECK(index.segments.size() >= 2);  // at least one segment per track

  // Each entry points inside its segment file.
  for (const IndexEntry& entry : index.entries) {
    REQUIRE(entry.segment < index.segments.size());
    const SegmentInfo& segment = index.segments[entry.segment];
    std::filesystem::path path = std::filesystem::path(out) / segment.path;
    CHECK(std::filesystem::exists(path));
    std::uintmax_t size = std::filesystem::file_size(path);
    CHECK(entry.payload_offset + entry.payload_size <= size);
    CHECK(entry.payload_size > 0);
  }
}

TEST_CASE("segments are seekable and decode", "[index]") {
  TempDir dir;
  REQUIRE(generate_delivery(dir.path().string(),
                            GenerateOptions{Scenario::kNormal, 400, 13307}, true)
              .ok);
  Source source = load_source(dir.path().string());
  const std::string out = (dir.path() / "out").string();
  SegmentWriter writer;
  Error error;
  REQUIRE(writer.open(out, source.specs, false, error));
  MediaIndex index;
  REQUIRE(write_all(writer, source, index, error));

  for (const SegmentInfo& segment : index.segments) {
    std::filesystem::path path = std::filesystem::path(out) / segment.path;
    AVFormatContext* probe = nullptr;
    REQUIRE(avformat_open_input(&probe, path.string().c_str(), nullptr, nullptr) == 0);
    REQUIRE(avformat_find_stream_info(probe, nullptr) >= 0);
    CHECK(probe->nb_streams == 1);
    // Seeking within the segment must succeed.
    int64_t duration = probe->streams[0]->duration;
    if (duration == AV_NOPTS_VALUE) duration = 0;
    CHECK(av_seek_frame(probe, 0, duration / 2, AVSEEK_FLAG_BACKWARD) >= 0);
    avformat_close_input(&probe);
  }
}

TEST_CASE("random access by time and frame finds the right entries",
          "[index]") {
  TempDir dir;
  REQUIRE(generate_delivery(dir.path().string(),
                            GenerateOptions{Scenario::kNormal, 300, 13307}, true)
              .ok);
  Source source = load_source(dir.path().string());
  const std::string out = (dir.path() / "out").string();
  SegmentWriter writer;
  Error error;
  REQUIRE(writer.open(out, source.specs, false, error));
  MediaIndex index;
  REQUIRE(write_all(writer, source, index, error));
  REQUIRE(index.write(out, error));

  MediaIndex reloaded;
  REQUIRE(load_index(out, reloaded, error));
  CHECK(reloaded.entries.size() == index.entries.size());
  CHECK(reloaded.segments.size() == index.segments.size());

  const IndexEntry* by_time = find_by_time(reloaded, "video_main", 1000000000LL);
  REQUIRE(by_time != nullptr);
  CHECK(by_time->time_ns <= 1000000000LL);

  const IndexEntry* by_frame = find_by_frame(reloaded, "video_main", 40);
  REQUIRE(by_frame != nullptr);
  CHECK(by_frame->frame <= 40);
  CHECK(by_frame->track == "video_main");

  // Checksums survive the sidecar round-trip.
  CHECK(reloaded.entries.front().sha256 == index.entries.front().sha256);
}

TEST_CASE("refuses to overwrite existing segments without overwrite",
          "[index]") {
  TempDir dir;
  REQUIRE(generate_delivery(dir.path().string(),
                            GenerateOptions{Scenario::kNormal, 60, 13307}, true)
              .ok);
  Source source = load_source(dir.path().string());
  const std::string out = (dir.path() / "out").string();

  SegmentWriter first;
  Error error;
  REQUIRE(first.open(out, source.specs, false, error));
  MediaIndex ignored;
  REQUIRE(write_all(first, source, ignored, error));

  SegmentWriter second;
  CHECK_FALSE(second.open(out, source.specs, false, error));
  CHECK(error.code == "output_exists");
  CHECK(second.open(out, source.specs, true, error));
}

TEST_CASE("abort removes partial segments", "[index]") {
  TempDir dir;
  REQUIRE(generate_delivery(dir.path().string(),
                            GenerateOptions{Scenario::kNormal, 60, 13307}, true)
              .ok);
  Source source = load_source(dir.path().string());
  const std::string out = (dir.path() / "out").string();
  {
    SegmentWriter writer;
    Error error;
    REQUIRE(writer.open(out, source.specs, false, error));
    SegmentWriter::FrameInput input;
    input.time_ns = 0;
    input.frame = 0;
    input.frame_index = 0;
    input.keyframe = true;
    input.payload = source.packets[0][0].data;
    IndexEntry ignored;
    REQUIRE(writer.write(0, input, ignored, error));
    // Destroyed without finish() => abort path removes the open segment.
  }
  std::filesystem::path media = std::filesystem::path(out) / "media";
  if (std::filesystem::exists(media)) {
    std::size_t mp4_count = 0;
    for (const auto& entry : std::filesystem::directory_iterator(media)) {
      if (entry.path().extension() == ".mp4") ++mp4_count;
    }
    CHECK(mp4_count == 0);
  }
}

TEST_CASE("index rejects a segment path that escapes the directory",
          "[index]") {
  TempDir dir;
  const std::string out = (dir.path() / "out").string();
  std::filesystem::create_directories(out);
  REQUIRE(dir.write("out/index.json", R"({
    "version": 1,
    "segments": [{"track":"v","path":"../../etc/passwd","first_frame":0,
                  "frames":1,"start_ns":0,"end_ns":1,"sha256":"","bytes":1}],
    "entries": []
  })"));
  MediaIndex index;
  Error error;
  CHECK_FALSE(load_index(out, index, error));
  CHECK(error.code == "index_path_escape");
}

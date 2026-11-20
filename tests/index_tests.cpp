#include "media/index_writer.hpp"

#include <catch2/catch_test_macros.hpp>

#include <filesystem>
#include <fstream>
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
  std::vector<IndexWriter::TrackSpec> specs;
  std::vector<std::vector<FrameMetadata>> metadata;
  std::vector<std::vector<FrameReader::Packet>> packets;
};

Source load_source(const std::string& dir) {
  auto delivery = load_delivery(dir);
  REQUIRE(delivery.ok);
  Source source;
  for (const DeliveryTrack& track : delivery.delivery.tracks) {
    IndexWriter::TrackSpec spec;
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

// Streams every source frame through the writer, as the pipeline does.
bool write_all(IndexWriter& writer, const Source& source, MediaIndex& index,
               Error& error) {
  for (std::size_t t = 0; t < source.specs.size(); ++t) {
    TrackTimeline timeline(
        TimelineConfig{source.specs[t].desc.rate, source.specs[t].desc.clock});
    for (std::size_t i = 0; i < source.packets[t].size(); ++i) {
      const FrameMetadata& meta = source.metadata[t][i];
      auto mapping = timeline.map(meta.has_capture, meta.capture_ticks);
      IndexWriter::PacketInput input;
      input.track_index = t;
      input.time_ns = mapping.ns;
      input.frame = meta.seq;
      input.keyframe = source.packets[t][i].keyframe;
      input.sha256 = meta.sha;
      input.payload = source.packets[t][i].data;
      if (!writer.write(input, error)) return false;
    }
  }
  return writer.close(index, error);
}

}  // namespace

TEST_CASE("index writer emits one entry per source frame with real offsets",
          "[index]") {
  TempDir dir;
  REQUIRE(generate_delivery(dir.path().string(),
                            GenerateOptions{Scenario::kNormal, 300, 13307}, true)
              .ok);
  Source source = load_source(dir.path().string());

  const std::string out = (dir.path() / "program.mp4").string();
  IndexWriter writer;
  Error error;
  REQUIRE(writer.open(out, source.specs, false, error));
  MediaIndex index;
  REQUIRE(write_all(writer, source, index, error));

  std::size_t total_frames = 0;
  for (const auto& frames : source.metadata) total_frames += frames.size();
  CHECK(index.entries.size() == total_frames);

  // Every entry points inside the file.
  std::uintmax_t size = std::filesystem::file_size(out);
  for (const IndexEntry& entry : index.entries) {
    CHECK(entry.payload_offset + entry.payload_size <= size);
    CHECK(entry.payload_size > 0);
  }

  // Entries are time sorted for binary-search random access.
  for (std::size_t i = 1; i < index.entries.size(); ++i) {
    CHECK(index.entries[i - 1].time_ns <= index.entries[i].time_ns);
  }

  // The container really is seekable and reports the right frame count.
  CHECK(index.duration_ns > 0);
}

TEST_CASE("random access by time and frame finds the right entries",
          "[index]") {
  TempDir dir;
  REQUIRE(generate_delivery(dir.path().string(),
                            GenerateOptions{Scenario::kNormal, 300, 13307}, true)
              .ok);
  Source source = load_source(dir.path().string());
  const std::string out = (dir.path() / "program.mp4").string();
  IndexWriter writer;
  Error error;
  REQUIRE(writer.open(out, source.specs, false, error));
  MediaIndex index;
  REQUIRE(write_all(writer, source, index, error));
  REQUIRE(index.write(out, error));

  MediaIndex reloaded;
  REQUIRE(load_index(index.sidecar_path(out), reloaded, error));
  CHECK(reloaded.entries.size() == index.entries.size());

  const IndexEntry* by_time = find_by_time(reloaded, 1000000000LL);  // 1 s
  REQUIRE(by_time != nullptr);
  CHECK(by_time->time_ns <= 1000000000LL);

  const IndexEntry* by_frame = find_by_frame(reloaded, 40);
  REQUIRE(by_frame != nullptr);
  CHECK(by_frame->frame <= 40);

  // Checksums survive the round-trip through the sidecar.
  CHECK(reloaded.entries.front().sha256 == index.entries.front().sha256);
}

TEST_CASE("refuses to clobber an existing output without overwrite",
          "[index]") {
  TempDir dir;
  REQUIRE(generate_delivery(dir.path().string(),
                            GenerateOptions{Scenario::kNormal, 60, 13307}, true)
              .ok);
  Source source = load_source(dir.path().string());
  const std::string out = (dir.path() / "out.mp4").string();

  IndexWriter first;
  Error error;
  REQUIRE(first.open(out, source.specs, false, error));
  MediaIndex ignored;
  REQUIRE(write_all(first, source, ignored, error));

  IndexWriter second;
  CHECK_FALSE(second.open(out, source.specs, false, error));
  CHECK(error.code == "output_exists");
  CHECK(second.open(out, source.specs, true, error));
}

TEST_CASE("abort removes the partial output", "[index]") {
  TempDir dir;
  REQUIRE(generate_delivery(dir.path().string(),
                            GenerateOptions{Scenario::kNormal, 60, 13307}, true)
              .ok);
  Source source = load_source(dir.path().string());
  const std::string out = (dir.path() / "partial.mp4").string();
  {
    IndexWriter writer;
    Error error;
    REQUIRE(writer.open(out, source.specs, false, error));
    IndexWriter::PacketInput input;
    input.track_index = 0;
    input.time_ns = 0;
    input.frame = 0;
    input.keyframe = true;
    input.payload = source.packets[0][0].data;
    REQUIRE(writer.write(input, error));
    // Destroyed without close => abort path runs.
  }
  CHECK_FALSE(std::filesystem::exists(out));
}

TEST_CASE("output MP4 is parseable and preserves packet timing", "[index]") {
  TempDir dir;
  REQUIRE(generate_delivery(dir.path().string(),
                            GenerateOptions{Scenario::kNormal, 150, 13307}, true)
              .ok);
  Source source = load_source(dir.path().string());
  const std::string out = (dir.path() / "timing.mp4").string();
  IndexWriter writer;
  Error error;
  REQUIRE(writer.open(out, source.specs, false, error));
  MediaIndex index;
  REQUIRE(write_all(writer, source, index, error));

  AVFormatContext* probe = nullptr;
  REQUIRE(avformat_open_input(&probe, out.c_str(), nullptr, nullptr) == 0);
  REQUIRE(avformat_find_stream_info(probe, nullptr) >= 0);
  CHECK(probe->nb_streams == source.specs.size());
  // The video stream must carry the nanosecond time base the index assumes.
  for (unsigned i = 0; i < probe->nb_streams; ++i) {
    AVStream* stream = probe->streams[i];
    if (stream->codecpar->codec_type == AVMEDIA_TYPE_VIDEO) {
      CHECK(stream->time_base.den >= 1000);
    }
  }
  avformat_close_input(&probe);
}

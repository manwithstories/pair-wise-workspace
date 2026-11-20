#include "relay/frame_reader.hpp"

#include <algorithm>
#include <cctype>
#include <climits>
#include <cstring>
#include <fstream>
#include <sstream>
#include <stdexcept>
#include <unordered_set>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/imgutils.h>
#include <libavutil/pixdesc.h>
}

#include "relay/hash.hpp"

namespace relay {
namespace {

constexpr std::uint64_t kMaxPayloadBytes = 64ull * 1024ull * 1024ull;

struct MemReader {
  const std::uint8_t* data = nullptr;
  int size = 0;
  int pos = 0;
};

int mem_read(void* opaque, std::uint8_t* buf, int buf_size) {
  auto* reader = static_cast<MemReader*>(opaque);
  const int remaining = reader->size - reader->pos;
  if (remaining <= 0) {
    return AVERROR_EOF;
  }
  const int take = std::min(buf_size, remaining);
  std::memcpy(buf, reader->data + reader->pos, static_cast<std::size_t>(take));
  reader->pos += take;
  return take;
}

std::string trim(std::string value) {
  while (!value.empty() && std::isspace(static_cast<unsigned char>(value.front()))) {
    value.erase(value.begin());
  }
  while (!value.empty() && std::isspace(static_cast<unsigned char>(value.back()))) {
    value.pop_back();
  }
  return value;
}

bool parse_u64(const std::string& text, std::uint64_t& value) {
  try {
    std::size_t used = 0;
    value = std::stoull(text, &used, 10);
    return used == text.size();
  } catch (const std::exception&) {
    return false;
  }
}

bool parse_int(const std::string& text, int& value) {
  try {
    std::size_t used = 0;
    value = std::stoi(text, &used, 10);
    return used == text.size();
  } catch (const std::exception&) {
    return false;
  }
}

struct Header {
  std::string track;
  Timebase timebase;
  std::vector<TrackId> dependencies;
  bool required = true;
  std::uint64_t count = 0;
  bool have_count = false;
};

bool parse_header(std::istream& in, Header& header, std::string& reason) {
  std::string magic;
  std::getline(in, magic);
  if (trim(magic) != "RELAYFRAMES 1") {
    reason = "bad frame stream magic";
    return false;
  }
  std::string line;
  bool have_track = false;
  while (std::getline(in, line)) {
    line = trim(line);
    if (line.empty()) {
      continue;
    }
    std::istringstream tokens(line);
    std::string key;
    tokens >> key;
    if (key == "track") {
      tokens >> header.track;
      have_track = true;
    } else if (key == "timebase") {
      std::string value;
      tokens >> value;
      const auto slash = value.find('/');
      if (slash == std::string::npos ||
          !parse_int(value.substr(0, slash), header.timebase.fps_num) ||
          !parse_int(value.substr(slash + 1), header.timebase.fps_den) ||
          header.timebase.fps_num <= 0 || header.timebase.fps_den <= 0) {
        reason = "bad timebase header";
        return false;
      }
    } else if (key == "requires") {
      while (tokens >> key) {
        header.dependencies.push_back(key);
      }
    } else if (key == "required") {
      int flag = 1;
      tokens >> flag;
      header.required = flag != 0;
    } else if (key == "count") {
      if (!parse_u64(line.substr(key.size()), header.count)) {
        reason = "bad frame count";
        return false;
      }
      header.have_count = true;
    } else if (key == "FRAME") {
      reason = "frame record before count header";
      return false;
    } else {
      reason = "unknown header field: " + key;
      return false;
    }
    if (have_track && header.have_count && !header.track.empty()) {
      return true;
    }
  }
  reason = "truncated frame stream header";
  return false;
}

}  // namespace

bool verify_decoded_frame(const std::vector<std::uint8_t>& payload,
                          const FrameRecord& record, std::string& reason) {
  if (payload.empty()) {
    reason = "empty frame payload";
    return false;
  }
  if (payload.size() > static_cast<std::size_t>(INT_MAX)) {
    reason = "frame payload exceeds FFmpeg packet limit";
    return false;
  }
  MemReader reader{payload.data(), static_cast<int>(payload.size()), 0};
  unsigned char* avio_buffer =
      static_cast<unsigned char*>(av_malloc(4096));
  if (avio_buffer == nullptr) {
    reason = "av_malloc failed";
    return false;
  }
  AVIOContext* avio = avio_alloc_context(avio_buffer, 4096, 0, &reader,
                                         mem_read, nullptr, nullptr);
  if (avio == nullptr) {
    av_free(avio_buffer);
    reason = "avio_alloc_context failed";
    return false;
  }
  AVFormatContext* format = avformat_alloc_context();
  format->pb = avio;
  format->flags |= AVFMT_FLAG_CUSTOM_IO;
  const int opened = avformat_open_input(&format, nullptr, nullptr, nullptr);
  if (opened < 0) {
    av_free(avio->buffer);
    avio_context_free(&avio);
    reason = "libavformat could not open decoded payload";
    return false;
  }
  if (avformat_find_stream_info(format, nullptr) < 0 ||
      format->nb_streams == 0) {
    avformat_close_input(&format);
    av_free(avio->buffer);
    avio_context_free(&avio);
    reason = "libavformat found no frame stream";
    return false;
  }
  const int stream_index = av_find_best_stream(
      format, AVMEDIA_TYPE_VIDEO, -1, -1, nullptr, 0);
  if (stream_index < 0) {
    avformat_close_input(&format);
    av_free(avio->buffer);
    avio_context_free(&avio);
    reason = "no video stream in payload";
    return false;
  }
  const AVCodecParameters* params = format->streams[stream_index]->codecpar;
  const AVCodec* codec = avcodec_find_decoder(params->codec_id);
  AVCodecContext* ctx = avcodec_alloc_context3(codec);
  bool ok = codec != nullptr && ctx != nullptr &&
            avcodec_parameters_to_context(ctx, params) >= 0 &&
            avcodec_open2(ctx, codec, nullptr) >= 0;
  bool decoded = false;
  if (ok) {
    AVPacket* packet = av_packet_alloc();
    AVFrame* frame = av_frame_alloc();
    while (av_read_frame(format, packet) >= 0) {
      if (packet->stream_index == stream_index &&
          avcodec_send_packet(ctx, packet) >= 0) {
        while (avcodec_receive_frame(ctx, frame) >= 0) {
          const char* pix = av_get_pix_fmt_name(
              static_cast<AVPixelFormat>(frame->format));
          if (frame->width != record.width || frame->height != record.height) {
            reason = "decoded dimensions disagree with metadata";
          } else if (pix == nullptr || record.pixel_format != pix) {
            reason = "decoded pixel format disagrees with metadata";
          } else {
            decoded = true;
          }
          av_frame_unref(frame);
        }
      }
      av_packet_unref(packet);
    }
    av_packet_free(&packet);
    av_frame_free(&frame);
  } else {
    reason = "FFmpeg decoder unavailable";
  }
  avcodec_free_context(&ctx);
  avformat_close_input(&format);
  av_free(avio->buffer);
  avio_context_free(&avio);
  if (!decoded && reason.empty()) {
    reason = "FFmpeg decoded no frame";
  }
  return decoded;
}

ReadResult read_frame_inputs(const std::filesystem::path& input_dir,
                             ErrorSink& errors) {
  namespace fs = std::filesystem;
  ReadResult result;
  std::vector<fs::path> frame_files;
  if (!fs::exists(input_dir)) {
    errors.fail("read", "", 0, "input directory missing: " + input_dir.string());
    return result;
  }
  for (const auto& entry : fs::directory_iterator(input_dir)) {
    if (entry.is_regular_file() && entry.path().extension() == ".frames") {
      frame_files.push_back(entry.path());
    }
  }
  std::sort(frame_files.begin(), frame_files.end());
  if (frame_files.empty()) {
    errors.fail("read", "", 0, "no .frames input found in " + input_dir.string());
    return result;
  }
  Sha256Stream digest;
  for (const auto& frames_path : frame_files) {
    std::ifstream in(frames_path, std::ios::binary);
    Header header;
    std::string reason;
    if (!in || !parse_header(in, header, reason)) {
      errors.fail("read", frames_path.stem().string(), 0,
                  "header rejected: " + reason);
      continue;
    }
    TrackInput track;
    track.spec.id = header.track;
    track.spec.kind = "video";
    track.spec.depends_on = header.dependencies;
    track.spec.required = header.required;
    track.timebase = header.timebase;

    std::string line;
    std::uint64_t index = 0;
    bool truncated = false;
    while (std::getline(in, line)) {
      if (line.empty()) {
        continue;
      }
      std::istringstream tokens(line);
      std::string keyword;
      tokens >> keyword;
      if (keyword != "FRAME") {
        errors.fail("read", header.track, index, "unexpected record: " + keyword);
        truncated = true;
        break;
      }
      FrameRecord record;
      record.track = header.track;
      record.source_frame = index;
      std::string seq_text;
      std::string raw_text;
      std::string key_text;
      std::string size_text;
      if (!(tokens >> seq_text >> raw_text >> key_text >> record.pixel_format >>
            record.width >> record.height >> record.exposure.iso >>
            record.exposure.shutter_us >> record.exposure.aperture >>
            record.sha256 >> size_text)) {
        errors.fail("read", header.track, index, "malformed FRAME metadata");
        truncated = true;
        break;
      }
      std::uint64_t seq = 0;
      std::uint64_t raw = 0;
      std::uint64_t size = 0;
      int key = 0;
      if (!parse_u64(seq_text, seq) || !parse_u64(raw_text, raw) ||
          !parse_int(key_text, key) || !parse_u64(size_text, size) ||
          seq > UINT32_MAX || raw > UINT32_MAX ||
          size > kMaxPayloadBytes) {
        errors.fail("read", header.track, index, "invalid FRAME field range");
        truncated = true;
        break;
      }
      record.sequence = static_cast<std::uint32_t>(seq);
      record.raw_timestamp_us = static_cast<std::uint32_t>(raw);
      record.keyframe = key != 0;
      record.payload_bytes = static_cast<std::size_t>(size);
      std::vector<std::uint8_t> payload(static_cast<std::size_t>(size));
      if (size > 0) {
        in.read(reinterpret_cast<char*>(payload.data()),
                static_cast<std::streamsize>(size));
        if (static_cast<std::uint64_t>(in.gcount()) != size) {
          errors.fail("read", header.track, index, "truncated frame payload");
          truncated = true;
          break;
        }
      }
      char newline = 0;
      if (!in.get(newline) || newline != '\n') {
        errors.fail("read", header.track, index, "missing payload terminator");
        truncated = true;
        break;
      }
      CatalogFrame catalog_frame;
      catalog_frame.record = record;
      if (!is_hex_digest(record.sha256)) {
        catalog_frame.issue = "malformed SHA-256 digest";
      } else {
        const std::string actual = sha256_hex(payload);
        if (actual != record.sha256) {
          catalog_frame.issue = "SHA-256 mismatch (expected " + record.sha256 +
                                ", got " + actual + ")";
        } else if (!verify_decoded_frame(payload, record, reason)) {
          catalog_frame.issue = "FFmpeg decode rejected frame: " + reason;
          ++track.decode_failures;
          ++result.decode_failures;
        } else {
          catalog_frame.verified = true;
        }
      }
      if (!catalog_frame.verified && catalog_frame.issue.find("SHA-256") !=
                                          std::string::npos) {
        ++track.checksum_failures;
        ++result.checksum_failures;
      }
      if (!catalog_frame.verified) {
        errors.fail("read", header.track, index, catalog_frame.issue);
      }
      track.catalog.push_back(std::move(catalog_frame));
      ++index;
    }
    if (!truncated && header.have_count && index != header.count) {
      errors.fail("read", header.track, index,
                  "frame count mismatch: header=" + std::to_string(header.count) +
                      " parsed=" + std::to_string(index));
    }
    if (track.catalog.empty()) {
      errors.warn("read", header.track, 0,
                  "track is empty; no segment or index will be invented");
    }
    fs::path arrival_path = frames_path;
    arrival_path.replace_extension(".arrival");
    std::ifstream arrival_in(arrival_path);
    if (arrival_in) {
      std::uint64_t expected_index = 0;
      std::string arrival_line;
      while (std::getline(arrival_in, arrival_line)) {
        if (trim(arrival_line).empty()) {
          continue;
        }
        std::istringstream tokens(arrival_line);
        std::uint64_t arrival_index = 0;
        std::uint64_t source = 0;
        if (!(tokens >> arrival_index >> source) ||
            arrival_index != expected_index) {
          errors.fail("read", header.track, source,
                      "malformed or non-monotonic arrival record");
          break;
        }
        ++expected_index;
        if (source >= track.catalog.size()) {
          errors.fail("read", header.track, source,
                      "arrival references unknown source frame");
          continue;
        }
        track.arrivals.push_back(Arrival{arrival_index, source});
      }
    } else {
      errors.fail("read", header.track, 0,
                  "arrival schedule missing: " + arrival_path.string());
    }
    std::ifstream raw_frames(frames_path, std::ios::binary);
    std::ostringstream frames_content;
    frames_content << raw_frames.rdbuf();
    digest.update(frames_path.filename().generic_string());
    digest.update(frames_content.str());
    std::ifstream raw_arrival(arrival_path, std::ios::binary);
    std::ostringstream arrival_content;
    arrival_content << raw_arrival.rdbuf();
    digest.update(arrival_path.filename().generic_string());
    digest.update(arrival_content.str());
    track.file_digest = sha256_hex(frames_content.str());
    result.tracks.push_back(std::move(track));
  }
  result.input_digest = digest.finish_hex();
  return result;
}

}  // namespace relay

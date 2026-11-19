#include "fixture.hpp"

#include <algorithm>
#include <cmath>
#include <cstring>
#include <fstream>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavutil/opt.h>
}

#include "audio_restore.hpp"
#include "file_io.hpp"
#include "json.hpp"

namespace media_restore {
namespace {

constexpr int kFixtureRate = 90000;

Diagnostic fail(ErrorCode code, std::string message) {
  Diagnostic diagnostic;
  diagnostic.code = code;
  diagnostic.message = std::move(message);
  return diagnostic;
}

const char* encoder_name(Codec codec) {
  switch (codec) {
    case Codec::h264: return "libx264";
    case Codec::h265: return "libx265";
    case Codec::av1: return "libsvtav1";
  }
  return "";
}

// A deterministic moving pattern: three coloured bars slide horizontally and
// a brightness ramp makes every frame unique, so P-frames carry real motion.
void fill_frame(AVFrame* frame, int index, int width, int height) {
  for (int y = 0; y < height; ++y) {
    for (int x = 0; x < width; ++x) {
      const int bar = ((x + index * 3) / (width / 3 + 1)) % 3;
      const int luma = 40 + bar * 60 + (y * 80) / height;
      frame->data[0][y * frame->linesize[0] + x] = static_cast<uint8_t>(luma);
    }
  }
  for (int y = 0; y < height / 2; ++y) {
    for (int x = 0; x < width / 2; ++x) {
      frame->data[1][y * frame->linesize[1] + x] =
          static_cast<uint8_t>(96 + ((x + index * 2) % 64));
      frame->data[2][y * frame->linesize[2] + x] =
          static_cast<uint8_t>(96 + ((y + index * 2) % 64));
    }
  }
}

bool append_packet(std::ofstream& out, const AVPacket* packet) {
  out.write(reinterpret_cast<const char*>(packet->data), packet->size);
  return static_cast<bool>(out);
}

}  // namespace

bool encode_raw_fixture(Codec codec, const std::filesystem::path& path,
                        int frames, int gop, int width, int height,
                        std::vector<GeneratedKeyframe>* keyframes,
                        Diagnostic* error) {
  const AVCodec* encoder = avcodec_find_encoder_by_name(encoder_name(codec));
  if (encoder == nullptr) {
    *error = fail(ErrorCode::internal,
                  std::string("encoder unavailable: ") + encoder_name(codec));
    return false;
  }
  AVCodecContext* context = avcodec_alloc_context3(encoder);
  if (context == nullptr) {
    *error = fail(ErrorCode::internal, "cannot allocate encoder context");
    return false;
  }
  context->width = width;
  context->height = height;
  context->time_base = AVRational{1, 30};
  context->framerate = AVRational{30, 1};
  context->pix_fmt = AV_PIX_FMT_YUV420P;
  context->gop_size = gop;
  context->max_b_frames = 0;
  context->thread_count = 1;  // deterministic packet layout
  AVDictionary* options = nullptr;
  const std::string gop_text = std::to_string(gop);
  if (codec == Codec::h264) {
    av_dict_set(&options, "preset", "ultrafast", 0);
    const std::string params = "bframes=0:keyint=" + gop_text + ":scenecut=0";
    av_dict_set(&options, "x264-params", params.c_str(), 0);
  } else if (codec == Codec::h265) {
    av_dict_set(&options, "preset", "ultrafast", 0);
    const std::string params =
        "log-level=none:bframes=0:scenecut=0:keyint=" + gop_text;
    av_dict_set(&options, "x265-params", params.c_str(), 0);
  } else {
    av_dict_set(&options, "preset", "12", 0);
    const std::string params = "keyint=" + gop_text + ":scd=0";
    av_dict_set(&options, "svtav1-params", params.c_str(), 0);
  }
  int result = avcodec_open2(context, encoder, &options);
  av_dict_free(&options);
  if (result < 0) {
    avcodec_free_context(&context);
    *error = fail(ErrorCode::internal,
                  std::string("cannot open encoder: ") + encoder_name(codec));
    return false;
  }

  std::ofstream out(path, std::ios::binary | std::ios::trunc);
  if (!out) {
    avcodec_free_context(&context);
    *error = fail(ErrorCode::io, "cannot open fixture output");
    return false;
  }
  AVFrame* frame = av_frame_alloc();
  AVPacket* packet = av_packet_alloc();
  bool ok = frame != nullptr && packet != nullptr;
  if (ok) {
    frame->format = AV_PIX_FMT_YUV420P;
    frame->width = width;
    frame->height = height;
    ok = av_frame_get_buffer(frame, 32) >= 0;
  }
  int64_t offset = 0;
  const auto drain = [&]() {
    while (true) {
      const int received = avcodec_receive_packet(context, packet);
      if (received == AVERROR(EAGAIN) || received == AVERROR_EOF) return true;
      if (received < 0) return false;
      if ((packet->flags & AV_PKT_FLAG_KEY) != 0 && packet->pts >= 0) {
        GeneratedKeyframe keyframe;
        keyframe.tick = packet->pts * (kFixtureRate / 30);
        keyframe.offset = offset;
        keyframe.length = packet->size;
        keyframes->push_back(keyframe);
      }
      if (!append_packet(out, packet)) return false;
      offset += packet->size;
      av_packet_unref(packet);
    }
  };
  for (int i = 0; ok && i < frames; ++i) {
    if (av_frame_make_writable(frame) < 0) {
      ok = false;
      break;
    }
    fill_frame(frame, i, width, height);
    frame->pts = i;
    if (avcodec_send_frame(context, frame) < 0) {
      ok = false;
      break;
    }
    ok = drain();
    if (ok && offset > 32 * 1024 * 1024) {
      *error = fail(ErrorCode::limit_exceeded, "fixture stream grew too large");
      ok = false;
    }
  }
  if (ok) {
    avcodec_send_frame(context, nullptr);
    ok = drain();
  }
  av_packet_free(&packet);
  av_frame_free(&frame);
  avcodec_free_context(&context);
  if (!ok) {
    *error = fail(ErrorCode::internal,
                  std::string("encoding failed for ") + codec_name(codec));
    return false;
  }
  out.flush();
  if (!out) {
    *error = fail(ErrorCode::io, "cannot write fixture stream");
    return false;
  }
  if (keyframes->empty()) {
    *error = fail(ErrorCode::internal, "encoder produced no keyframes");
    return false;
  }
  return true;
}

bool write_audio_fixture(const std::filesystem::path& path, int sample_rate,
                         const std::string& layout_name, int64_t frames,
                         double hz, double amplitude, bool silence,
                         Diagnostic* error) {
  PcmBuffer buffer;
  buffer.sample_rate = sample_rate;
  if (!parse_layout(layout_name, &buffer.layout)) {
    *error = fail(ErrorCode::audio_unsupported_layout,
                  "unknown fixture layout: " + layout_name);
    return false;
  }
  buffer.channels = buffer.layout.nb_channels;
  buffer.interleaved = false;
  buffer.frames = frames;
  buffer.data.assign(static_cast<size_t>(frames) * buffer.channels, 0.0f);
  if (!silence) {
    for (int c = 0; c < buffer.channels; ++c) {
      // Slight per-channel detune keeps mono and downmix paths honest.
      const double frequency = hz * (1.0 + 0.05 * c);
      for (int64_t i = 0; i < frames; ++i) {
        const double t = static_cast<double>(i) / sample_rate;
        const double envelope = 0.5 + 0.5 * std::sin(2 * M_PI * 0.4 * t);
        buffer.plane(c)[i] =
            static_cast<float>(amplitude * envelope *
                               std::sin(2 * M_PI * frequency * t));
      }
    }
  }
  std::string wav;
  std::vector<Diagnostic> diagnostics;
  if (!encode_wav(buffer, &wav, &diagnostics)) {
    *error = diagnostics.empty() ? fail(ErrorCode::io, "cannot encode fixture audio")
                                 : diagnostics.front();
    return false;
  }
  return write_file_atomic(path, wav, error);
}

namespace {

constexpr int kTicksPerSecond = 90000;

Json color_json(const std::string& source, const char* primaries,
                const char* transfer, const char* matrix, const char* range) {
  Json color = Json::object();
  if (primaries != nullptr) color.set("primaries", Json(primaries));
  if (transfer != nullptr) color.set("transfer", Json(transfer));
  if (matrix != nullptr) color.set("matrix", Json(matrix));
  if (range != nullptr) color.set("range", Json(range));
  color.set("source", Json(source));
  return color;
}

Json index_json(const std::string& path) {
  Json index = Json::object();
  index.set("path", Json(path));
  index.set("version", Json(int64_t{1}));
  return index;
}

Json quarantine_json(ErrorCode code) {
  Json quarantine = Json::object();
  quarantine.set("expected_code", Json(error_code_name(code)));
  return quarantine;
}

struct ReelSpec {
  std::string name;
  Codec codec;
  int frames;
  int gop;
  int64_t audio_rate;
  std::string audio_layout;
  const char* primaries;
  const char* transfer;
};

bool write_damaged_index(const std::filesystem::path& path,
                         const std::vector<IndexEntry>& entries,
                         std::vector<Diagnostic>* diagnostics) {
  Json root = Json::object();
  root.set("format", Json("media-archive-keyframe-index"));
  root.set("version", Json(int64_t{1}));
  Json list = Json::array();
  for (const auto& entry : entries) {
    Json item = Json::object();
    item.set("tick", Json(entry.tick));
    item.set("offset", Json(entry.offset));
    item.set("keyframe", Json(entry.keyframe));
    list.push(std::move(item));
  }
  root.set("entries", std::move(list));
  Diagnostic error;
  if (!write_file_atomic(path, root.dump(), &error)) {
    diagnostics->push_back(std::move(error));
    return false;
  }
  return true;
}

}  // namespace

const char* extension_for(Codec codec) {
  switch (codec) {
    case Codec::h264: return "h264";
    case Codec::h265: return "h265";
    case Codec::av1: return "obu";
  }
  return "bin";
}

bool generate_archive(const FixtureOptions& options,
                      std::vector<Diagnostic>* diagnostics) {
  const std::filesystem::path out = options.out_dir;
  const std::filesystem::path media = out / "media";
  Diagnostic error;
  if (!ensure_directory(media, &error)) {
    diagnostics->push_back(std::move(error));
    return false;
  }

  const ReelSpec reels[] = {
      {"reel_a", Codec::h264, 45, 15, 44100, "stereo", "bt709", "bt709"},
      {"reel_b", Codec::h265, 30, 10, 48000, "mono", "bt709", "bt709"},
      {"reel_c", Codec::av1, 30, 15, 32000, "5.1", "bt2020", "smpte2084"},
  };

  struct EncodedReel {
    ReelSpec spec;
    std::vector<GeneratedKeyframe> keyframes;
  };
  std::vector<EncodedReel> encoded;
  for (const auto& reel : reels) {
    const std::filesystem::path raw =
        media / (reel.name + "." + extension_for(reel.codec));
    std::vector<GeneratedKeyframe> keyframes;
    if (!encode_raw_fixture(reel.codec, raw, reel.frames, reel.gop, 160, 90,
                            &keyframes, &error)) {
      diagnostics->push_back(std::move(error));
      return false;
    }
    // The external index file is written from real encoder keyframes.
    std::vector<IndexEntry> entries;
    for (const auto& keyframe : keyframes) {
      entries.push_back({keyframe.tick, keyframe.offset, true});
    }
    if (!write_damaged_index(media / (reel.name + ".idx"), entries, diagnostics)) {
      return false;
    }
    const int64_t audio_frames = reel.audio_rate / 2;  // matches 0.5 s segment
    if (!write_audio_fixture(media / (reel.name + ".wav"), static_cast<int>(reel.audio_rate),
                             reel.audio_layout, audio_frames,
                             440.0 + 220.0 * encoded.size(), 0.35, false, &error)) {
      diagnostics->push_back(std::move(error));
      return false;
    }
    encoded.push_back({reel, std::move(keyframes)});
  }
  if (!write_audio_fixture(media / "reel_silent.wav", 48000, "stereo", 96000, 0.0,
                           0.0, true, &error)) {
    diagnostics->push_back(std::move(error));
    return false;
  }

  Json root = Json::object();
  root.set("format", Json("media-archive-manifest"));
  root.set("version", Json(int64_t{1}));
  Json time_base = Json::object();
  time_base.set("num", Json("1"));
  time_base.set("den", Json(std::to_string(kTicksPerSecond)));
  root.set("time_base", std::move(time_base));
  Json tracks = Json::array();

  for (const auto& item : encoded) {
    Json track = Json::object();
    track.set("id", Json(item.spec.name));
    track.set("kind", Json("video"));
    Json depends = Json::array();
    if (item.spec.name == "reel_b") depends.push(Json("reel_a"));
    if (item.spec.name == "reel_c") depends.push(Json("reel_b"));
    track.set("depends_on", std::move(depends));
    Json segments = Json::array();
    const int64_t segment_ticks = kTicksPerSecond / 2;
    const int64_t total_ticks =
        static_cast<int64_t>(item.spec.frames) * (kTicksPerSecond / 30);
    for (int64_t start = 0, index = 0; start < total_ticks;
         start += segment_ticks, ++index) {
      Json segment = Json::object();
      segment.set("id", Json("seg-000" + std::to_string(index)));
      segment.set("start", Json(Rational(start, kTicksPerSecond).to_string()));
      segment.set("duration", Json(Rational(segment_ticks, kTicksPerSecond).to_string()));
      segment.set("media", Json("media/" + item.spec.name + "." +
                                extension_for(item.spec.codec)));
      segment.set("codec", Json(codec_name(item.spec.codec)));
      segment.set("color", color_json(item.spec.name, item.spec.primaries,
                                      item.spec.transfer, "bt709", "tv"));
      segment.set("index", index_json("media/" + item.spec.name + ".idx"));
      if (index == 0) {
        Json audio = Json::object();
        audio.set("sample_rate", Json(item.spec.audio_rate));
        audio.set("layout", Json(item.spec.audio_layout));
        // A small positive offset exercises sample-accurate padding.
        audio.set("offset", Json(Rational(1, 1000).to_string()));
        audio.set("media", Json("media/" + item.spec.name + ".wav"));
        segment.set("audio", std::move(audio));
      }
      segments.push(std::move(segment));
    }
    track.set("segments", std::move(segments));
    tracks.push(std::move(track));
  }

  {
    Json track = Json::object();
    track.set("id", Json("reel_silent"));
    track.set("kind", Json("audio"));
    track.set("depends_on", Json::array());
    Json segments = Json::array();
    Json segment = Json::object();
    segment.set("id", Json("seg-0000"));
    segment.set("start", Json("0/1"));
    segment.set("duration", Json("2/1"));
    segment.set("media", Json("media/reel_silent.wav"));
    segment.set("color", color_json("none", nullptr, nullptr, nullptr, nullptr));
    Json audio = Json::object();
    audio.set("sample_rate", Json(int64_t{48000}));
    audio.set("layout", Json("stereo"));
    audio.set("offset", Json("0/1"));
    audio.set("media", Json("media/reel_silent.wav"));
    segment.set("audio", std::move(audio));
    segments.push(std::move(segment));
    track.set("segments", std::move(segments));
    tracks.push(std::move(track));
  }

  if (options.include_damaged) {
    // The damaged track uses its own copy of the stream so a defect in one
    // fixture never contaminates the healthy reels' hash accounting.
    const std::filesystem::path damaged_media = media / "damaged";
    if (!ensure_directory(damaged_media, &error)) {
      diagnostics->push_back(std::move(error));
      return false;
    }
    std::vector<GeneratedKeyframe> tiny;
    if (!encode_raw_fixture(Codec::h264, damaged_media / "tiny.h264", 9, 3, 96,
                            54, &tiny, &error)) {
      diagnostics->push_back(std::move(error));
      return false;
    }
    if (tiny.size() < 2) {
      diagnostics->push_back(Diagnostic{.code = ErrorCode::internal,
                                        .message = "damaged fixture needs two keyframes",
                                        .location = {}});
      return false;
    }
    // A byte pattern with no recognisable start code or OBU header.
    const char kUnknownContent[] = {'M', 'P', 'E', 'G', '2', '-', 'S', 'I', 'G', 'N',
                                    'A', 'T', 'U', 'R', 'E', '\0'};
    const std::string unknown_bytes(kUnknownContent, sizeof(kUnknownContent));
    if (!write_file_atomic(damaged_media / "unknown.bin", unknown_bytes, &error)) {
      diagnostics->push_back(std::move(error));
      return false;
    }

    struct DamagedDef {
      std::string id;
      std::vector<IndexEntry> entries;
      bool unknown_content = false;
      ErrorCode expected;
      bool overlap = false;
    };
    std::vector<DamagedDef> defects;
    // Bytes that match no supported bitstream signature, declared as H.264:
    // content sniffing must reject this with a located error.
    DamagedDef unknown_codec;
    unknown_codec.id = "bad-codec";
    unknown_codec.unknown_content = true;
    unknown_codec.expected = ErrorCode::unknown_codec;
    defects.push_back(unknown_codec);

    DamagedDef descending;
    descending.id = "bad-index-order";
    descending.entries = {{tiny[1].tick, tiny[1].offset, true},
                          {tiny[0].tick, tiny[0].offset, true}};
    descending.expected = ErrorCode::index_order;
    defects.push_back(descending);

    DamagedDef duplicate;
    duplicate.id = "bad-index-duplicate";
    duplicate.entries = {{0, tiny[0].offset, true}, {3000, tiny[0].offset, true}};
    duplicate.expected = ErrorCode::index_duplicate;
    defects.push_back(duplicate);

    DamagedDef negative;
    negative.id = "bad-index-negative";
    negative.entries = {{0, -4, true}};
    negative.expected = ErrorCode::index_negative_offset;
    defects.push_back(negative);

    DamagedDef out_of_range;
    out_of_range.id = "bad-index-range";
    out_of_range.entries = {{0, 1'000'000'000, true}};
    out_of_range.expected = ErrorCode::index_out_of_range;
    defects.push_back(out_of_range);

    DamagedDef overlap;
    overlap.id = "bad-overlap";
    overlap.entries = {{0, tiny[0].offset, true}};
    overlap.expected = ErrorCode::segment_overlap;
    overlap.overlap = true;
    defects.push_back(overlap);

    Json track = Json::object();
    track.set("id", Json("damaged"));
    track.set("kind", Json("video"));
    track.set("depends_on", Json::array());
    Json segments = Json::array();
    int64_t cursor = 0;
    for (const auto& defect : defects) {
      Json segment = Json::object();
      segment.set("id", Json(defect.id));
      segment.set("start", Json(Rational(cursor, kTicksPerSecond).to_string()));
      segment.set("duration", Json(Rational(kTicksPerSecond, kTicksPerSecond).to_string()));
      if (defect.overlap) {
        // Start exactly at the previous segment's start, so the overlap is
        // unambiguous and independent of rounding.
        segment.set("start", Json(Rational(cursor - kTicksPerSecond,
                                           kTicksPerSecond).to_string()));
      }
      segment.set("media", Json(defect.unknown_content
                                    ? "media/damaged/unknown.bin"
                                    : "media/damaged/tiny.h264"));
      segment.set("codec", Json("h264"));
      segment.set("color", color_json("damaged", "bt709", "bt709", "bt709", "tv"));
      segment.set("quarantine", quarantine_json(defect.expected));
      if (!defect.unknown_content) {
        Json index = Json::object();
        index.set("path", Json("media/damaged/" + defect.id + ".idx"));
        index.set("version", Json(int64_t{1}));
        segment.set("index", std::move(index));
        if (!write_damaged_index(damaged_media / (defect.id + ".idx"),
                                 defect.entries, diagnostics)) {
          return false;
        }
      }
      segments.push(std::move(segment));
      cursor += kTicksPerSecond + kTicksPerSecond / 2;
    }
    track.set("segments", std::move(segments));
    tracks.push(std::move(track));
  }

  root.set("tracks", std::move(tracks));
  if (!write_file_atomic(out / "archive.json", root.dump(), &error)) {
    diagnostics->push_back(std::move(error));
    return false;
  }
  // The manifest path is echoed so the caller can print it.
  return true;
}

}  // namespace media_restore

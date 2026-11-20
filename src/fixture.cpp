#include "relay/fixture.hpp"

#include <algorithm>
#include <fstream>
#include <random>
#include <sstream>
#include <stdexcept>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavcodec/codec_id.h>
#include <libavutil/frame.h>
#include <libavutil/pixfmt.h>
}

#include "relay/hash.hpp"

namespace relay::fixture {
namespace {

struct PngBytes {
  std::vector<std::uint8_t> data;
};

PngBytes encode_png(int width, int height, unsigned seed, int camera,
                    int frame_index) {
  const AVCodec* codec = avcodec_find_encoder(AV_CODEC_ID_PNG);
  if (codec == nullptr) {
    throw std::runtime_error("FFmpeg PNG encoder unavailable");
  }
  AVCodecContext* ctx = avcodec_alloc_context3(codec);
  if (ctx == nullptr) {
    throw std::runtime_error("avcodec_alloc_context3 failed");
  }
  ctx->width = width;
  ctx->height = height;
  ctx->pix_fmt = AV_PIX_FMT_RGB24;
  ctx->time_base = AVRational{1, 1};
  if (avcodec_open2(ctx, codec, nullptr) < 0) {
    avcodec_free_context(&ctx);
    throw std::runtime_error("avcodec_open2(PNG) failed");
  }
  AVFrame* frame = av_frame_alloc();
  if (frame == nullptr) {
    avcodec_free_context(&ctx);
    throw std::runtime_error("av_frame_alloc failed");
  }
  frame->format = AV_PIX_FMT_RGB24;
  frame->width = width;
  frame->height = height;
  if (av_frame_get_buffer(frame, 32) < 0) {
    av_frame_free(&frame);
    avcodec_free_context(&ctx);
    throw std::runtime_error("av_frame_get_buffer failed");
  }
  for (int y = 0; y < height; ++y) {
    auto* row = static_cast<std::uint8_t*>(frame->data[0]) +
                static_cast<std::ptrdiff_t>(y) * frame->linesize[0];
    for (int x = 0; x < width; ++x) {
      row[3 * x + 0] =
          static_cast<std::uint8_t>((x * 3 + frame_index + camera * 41 + seed) & 0xff);
      row[3 * x + 1] = static_cast<std::uint8_t>((y * 5 + camera * 17) & 0xff);
      row[3 * x + 2] =
          static_cast<std::uint8_t>((frame_index * 7 + x + y) & 0xff);
    }
  }
  std::vector<std::uint8_t> out;
  if (avcodec_send_frame(ctx, frame) < 0) {
    av_frame_free(&frame);
    avcodec_free_context(&ctx);
    throw std::runtime_error("avcodec_send_frame failed");
  }
  AVPacket* packet = av_packet_alloc();
  const bool ok = packet != nullptr && avcodec_receive_packet(ctx, packet) == 0;
  if (ok) {
    out.assign(packet->data, packet->data + packet->size);
  }
  av_packet_free(&packet);
  av_frame_free(&frame);
  avcodec_free_context(&ctx);
  if (!ok) {
    throw std::runtime_error("avcodec_receive_packet failed");
  }
  return PngBytes{std::move(out)};
}

struct ArrivalPlan {
  std::vector<std::uint64_t> ids;
  std::size_t duplicates = 0;
  std::size_t reordered = 0;
};

/// Builds a deterministic arrival order: reordered pairs, then duplicated
/// packets, with keyframe positions kept intact so the demo keeps seekable.
ArrivalPlan make_arrivals(int count, int duplicate_stride, int reorder_stride,
                          const std::vector<int>& protected_frames) {
  ArrivalPlan plan;
  for (int i = 0; i < count; ++i) {
    plan.ids.push_back(static_cast<std::uint64_t>(i));
  }
  auto protected_at = [&](int index) {
    return std::find(protected_frames.begin(), protected_frames.end(), index) !=
           protected_frames.end();
  };
  if (reorder_stride > 1) {
    for (std::size_t i = 1; i + 1 < plan.ids.size();
         i += static_cast<std::size_t>(reorder_stride)) {
      const int a = static_cast<int>(plan.ids[i]);
      const int b = static_cast<int>(plan.ids[i + 1]);
      if (protected_at(a) || protected_at(b) || protected_at(a - 1)) {
        continue;
      }
      std::swap(plan.ids[i], plan.ids[i + 1]);
      ++plan.reordered;
    }
  }
  if (duplicate_stride > 1) {
    for (std::size_t i = static_cast<std::size_t>(duplicate_stride);
         i < plan.ids.size(); i += static_cast<std::size_t>(duplicate_stride)) {
      plan.ids.insert(plan.ids.begin() + static_cast<std::ptrdiff_t>(i + 1),
                      plan.ids[i]);
      ++plan.duplicates;
      ++i;
    }
  }
  return plan;
}

std::uint32_t sequence_for(int index, int reset_at) {
  if (reset_at >= 0 && index >= reset_at) {
    return static_cast<std::uint32_t>(index - reset_at);
  }
  return static_cast<std::uint32_t>(index);
}

}  // namespace

GeneratedFixture generate(const GenerateOptions& options, ErrorSink& errors) {
  (void)errors;
  namespace fs = std::filesystem;
  if (options.cameras.empty()) {
    throw std::invalid_argument("generator requires at least one camera");
  }
  GeneratedFixture out;
  out.root = options.root;
  out.timebase = Timebase{30, 1};
  const fs::path inputs = options.root / "inputs";
  fs::create_directories(inputs);

  const int frame_us = static_cast<int>(out.timebase.frame_duration_us() + 0.5);
  const int epoch_cut = options.timestamp_wrap ? options.gop : -1;
  const int seq_cut = options.sequence_reset ? 3 * options.gop : -1;
  const std::int64_t base_raw =
      options.timestamp_wrap ? (kTimestampModulus - static_cast<std::int64_t>(epoch_cut) * frame_us)
                             : 1'000'000;
  const int frame_count = options.frames;

  for (std::size_t cam = 0; cam < options.cameras.size(); ++cam) {
    TrackSpec spec;
    spec.id = options.cameras[cam];
    spec.kind = "video";
    if (cam == 1 && options.cameras.size() > 1) {
      spec.depends_on = {options.cameras.front()};
    }
    out.tracks.push_back(spec);
  }

  for (std::size_t cam = 0; cam < options.cameras.size(); ++cam) {
    const std::string& track = options.cameras[cam];
    const std::size_t empty_index =
        options.empty_camera_index == static_cast<std::size_t>(-1)
            ? options.cameras.size() - 1
            : options.empty_camera_index;
    const bool empty = options.empty_track && cam == empty_index;
    const int count = empty ? 0 : frame_count;
    CameraFixture fixture;
    fixture.track = track;
    fixture.frames_path = inputs / (track + ".frames");
    fixture.arrival_path = inputs / (track + ".arrival");
    fixture.canonical_frames = static_cast<std::size_t>(count);

    const bool corrupt_here =
        options.corrupt && cam == options.corrupt_camera_index;
    std::ofstream frames(fixture.frames_path, std::ios::binary);
    frames << "RELAYFRAMES 1\n" << "track " << track << "\n"
           << "timebase " << out.timebase.fps_num << '/'
           << out.timebase.fps_den << '\n';
    if (cam == 1 && options.cameras.size() > 1) {
      // cam-b depends on cam-a: mirror the declared graph into the header
      // so the reader reconstructs the same dependency edges.
      frames << "requires " << options.cameras.front() << '\n';
    }
    frames << "count " << count << '\n';
    for (int i = 0; i < count; ++i) {
      const std::int64_t raw64 =
          (base_raw + static_cast<std::int64_t>(i) * frame_us) % kTimestampModulus;
      const std::uint32_t raw = static_cast<std::uint32_t>(raw64);
      const std::uint32_t seq = sequence_for(i, seq_cut);
      const bool key = i % options.gop == 0;
      PngBytes png = encode_png(options.width, options.height, options.seed,
                                static_cast<int>(cam), i);
      std::string digest = sha256_hex(png.data);
      if (corrupt_here && i == options.corrupt_frame && !png.data.empty()) {
        png.data[png.data.size() / 2] ^= 0x5a;
        fixture.corrupted_frames.push_back(static_cast<std::uint64_t>(i));
      }
      frames << "FRAME " << seq << ' ' << raw << ' ' << (key ? 1 : 0) << ' '
             << "rgb24 " << options.width << ' ' << options.height << ' '
             << 400 << ' ' << 1000 << ' ' << 2.8 << ' ' << digest << ' '
             << png.data.size() << '\n';
      frames.write(reinterpret_cast<const char*>(png.data.data()),
                   static_cast<std::streamsize>(png.data.size()));
      frames.put('\n');
    }
    frames.close();

    std::vector<int> lost;
    for (int b = 0; b < count; b += options.gop) {
      for (int k = 0; k < options.loss_burst; ++k) {
        const int idx = b + options.gop / 2 + k;
        if (idx < count && (idx % options.gop) != 0) {
          lost.push_back(idx);
        }
      }
    }
    if (options.lose_keyframe) {
      lost.push_back(options.gop);
    }

    std::vector<int> protected_frames;
    for (int i = 0; i < count; i += options.gop) {
      protected_frames.push_back(i);
    }
    ArrivalPlan plan =
        make_arrivals(count, options.duplicate_stride, options.reorder_stride,
                      protected_frames);
    const std::size_t before_loss = plan.ids.size();
    plan.ids.erase(std::remove_if(plan.ids.begin(), plan.ids.end(),
                                  [&](std::uint64_t id) {
                                    return std::find(lost.begin(), lost.end(),
                                                     static_cast<int>(id)) !=
                                           lost.end();
                                  }),
                   plan.ids.end());
    const std::size_t removed = before_loss - plan.ids.size();
    out.expected_duplicates += plan.duplicates;
    out.expected_lost += removed;
    out.expected_reordered += plan.reordered;

    fixture.arrivals = plan.ids.size();
    std::ofstream arrival(fixture.arrival_path);
    for (std::size_t i = 0; i < plan.ids.size(); ++i) {
      arrival << i << ' ' << plan.ids[i] << '\n';
    }
    out.cameras.push_back(std::move(fixture));
  }
  out.input_digest = directory_digest(options.root);
  return out;
}

std::string directory_digest(const std::filesystem::path& root) {
  namespace fs = std::filesystem;
  std::vector<fs::path> files;
  if (fs::exists(root)) {
    for (const auto& entry : fs::recursive_directory_iterator(root)) {
      if (entry.is_regular_file()) {
        files.push_back(entry.path());
      }
    }
  }
  std::sort(files.begin(), files.end());
  Sha256Stream stream;
  for (const auto& file : files) {
    std::ifstream in(file, std::ios::binary);
    std::ostringstream content;
    content << in.rdbuf();
    stream.update(fs::relative(file, root).generic_string());
    stream.update(std::string_view("\0", 1));
    stream.update(content.str());
  }
  return stream.finish_hex();
}

}  // namespace relay::fixture

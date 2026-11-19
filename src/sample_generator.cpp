#include "media/sample_generator.hpp"
#include "media/checksum.hpp"

#include <random>
#include <cmath>

namespace media {

namespace {

constexpr std::uint8_t kStartCode[4] = {0, 0, 0, 1};

// H.264/H.265 NAL unit types that mark an access unit as a key frame.
constexpr std::uint8_t kH264IdrNal = 5;
constexpr std::uint8_t kH265IdrNal = 19; // IDR_W_RADL

void emit_u32(std::vector<std::uint8_t>& out, std::uint32_t v) {
  for (int i = 3; i >= 0; --i)
    out.push_back(static_cast<std::uint8_t>((v >> (8 * i)) & 0xff));
}

// Annex B parameter-set NAL: a deterministic payload the parser will treat
// as a configuration unit; frame NALs follow in their own access units.
void append_nal(std::vector<std::uint8_t>& out, std::uint8_t nal_type,
                std::uint8_t nal_ref, const std::vector<std::uint8_t>& rbsp) {
  out.insert(out.end(), kStartCode, kStartCode + 4);
  std::uint8_t header = static_cast<std::uint8_t>((nal_type & 0x1f) << 3 |
                                                   (nal_ref & 0x03) << 5 | 1);
  out.push_back(header);
  out.insert(out.end(), rbsp.begin(), rbsp.end());
}

// AV1 temporal unit: length prefix + OBU sequence. OBU type 1 = sequence
// header, 3 = frame; the high bit of the first payload byte marks key frames.
void append_obu_unit(std::vector<std::uint8_t>& out,
                     const std::vector<std::uint8_t>& obu) {
  emit_u32(out, static_cast<std::uint32_t>(obu.size()));
  out.insert(out.end(), obu.begin(), obu.end());
}

std::vector<std::uint8_t> make_obu(std::uint8_t type, bool key,
                                   std::uint32_t frame_index,
                                   std::uint32_t body_len) {
  std::vector<std::uint8_t> obu;
  obu.push_back(static_cast<std::uint8_t>((type & 0xf) << 3)); // header
  obu.push_back(static_cast<std::uint8_t>(body_len + 1));      // 1-byte len
  obu.push_back(static_cast<std::uint8_t>(key ? 0x80 : 0x00)); // key marker
  for (std::uint32_t i = 0; i < body_len; ++i)
    obu.push_back(static_cast<std::uint8_t>((frame_index * 31 + i * 7) & 0xff));
  return obu;
}

void append_h26x_unit(std::vector<std::uint8_t>& out, CodecType codec,
                      bool key, std::uint32_t frame_index,
                      std::uint32_t body_len) {
  const std::uint8_t nal = key
      ? (codec == CodecType::H264 ? kH264IdrNal : kH265IdrNal)
      : (codec == CodecType::H264 ? 1 : 1); // non-IDR slice
  std::vector<std::uint8_t> rbsp;
  emit_u32(rbsp, frame_index);
  for (std::uint32_t i = 0; i < body_len; ++i)
    rbsp.push_back(static_cast<std::uint8_t>((frame_index * 17 + i * 13) & 0xff));
  append_nal(out, nal, key ? 3 : 2, rbsp);
}

} // namespace

SampleBundle generate_sample(const SampleConfig& cfg) {
  SampleBundle b;
  std::mt19937 rng(cfg.seed);
  const std::int64_t frame_dur_us =
      1'000'000LL * cfg.fps_den / cfg.fps_num;

  // --- Video: parameter sets first, then one access unit per frame ----------
  if (cfg.codec == CodecType::AV1) {
    append_obu_unit(b.video, make_obu(1, true, 0, 24)); // sequence header
  } else {
    std::vector<std::uint8_t> sps = {0x42, 0x00, 0x1e, static_cast<std::uint8_t>(cfg.seed & 0xff)};
    std::vector<std::uint8_t> pps = {0x44, 0x01};
    append_nal(b.video, codec_from_string("h264") == cfg.codec ? 7 : 33, 3, sps);
    append_nal(b.video, codec_from_string("h264") == cfg.codec ? 8 : 34, 3, pps);
  }

  b.frames.reserve(cfg.frame_count);
  for (std::uint32_t i = 0; i < cfg.frame_count; ++i) {
    const bool key = (i % cfg.gop) == 0;
    const std::uint32_t body_len = key ? 180 : 60 + (rng() % 40);
    const std::uint64_t offset = b.video.size();
    if (cfg.codec == CodecType::AV1) {
      append_obu_unit(b.video, make_obu(3, key, i, body_len));
    } else {
      append_h26x_unit(b.video, cfg.codec, key, i, body_len);
    }
    Frame f;
    f.codec = cfg.codec;
    f.index = i;
    f.pts_us = f.dts_us = static_cast<std::int64_t>(i) * frame_dur_us;
    f.keyframe = key;
    f.offset = offset;
    f.data.assign(b.video.begin() + static_cast<std::ptrdiff_t>(offset),
                  b.video.end());
    b.frames.push_back(std::move(f));
    if (key) {
      b.index.entries.push_back({i, offset,
                                 static_cast<std::int64_t>(i) * frame_dur_us});
    }
  }
  b.index.codec = cfg.codec;
  b.index.declared_size = b.video.size();

  // --- Audio: 20 ms PCM blocks over the same media clock --------------------
  const std::int64_t block_dur_us =
      1'000'000LL * cfg.audio_block_samples / cfg.audio_sample_rate;
  const std::uint32_t total_blocks = static_cast<std::uint32_t>(
      static_cast<std::int64_t>(cfg.frame_count) * frame_dur_us / block_dur_us);
  b.audio.reserve(total_blocks);
  for (std::uint32_t i = 0; i < total_blocks; ++i) {
    AudioBlock blk;
    blk.index = i;
    blk.pts_us = static_cast<std::int64_t>(i) * block_dur_us;
    blk.sample_rate = cfg.audio_sample_rate;
    blk.channels = 1;
    blk.samples.resize(cfg.audio_block_samples);
    for (std::uint32_t s = 0; s < cfg.audio_block_samples; ++s) {
      // Deterministic 440 Hz-ish tone plus seed offset.
      blk.samples[s] = static_cast<std::int16_t>(
          8000 * std::sin((i * cfg.audio_block_samples + s) * 0.0578) +
          (cfg.seed & 0x0f));
    }
    b.audio.push_back(std::move(blk));
  }

  b.input_hash = fnv1a64(b.video.data(), b.video.size());
  return b;
}

} // namespace media

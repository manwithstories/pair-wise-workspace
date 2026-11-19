#include <algorithm>
#include <cmath>
#include <cstring>
#include <fstream>
#include <iterator>
#include <utility>

extern "C" {
#include <libavutil/channel_layout.h>
#include <libavutil/opt.h>
#include <libswresample/swresample.h>
}

#include "media_archive/audio_sync.h"

namespace media_archive {
namespace {

constexpr char kMagic[8] = {'M', 'A', 'P', 'C', 'M', '1', 0, 0};

void put_u16(std::ofstream& f, uint16_t v) {
  char b[2] = {static_cast<char>(v & 0xFF),
               static_cast<char>((v >> 8) & 0xFF)};
  f.write(b, 2);
}
void put_u32(std::ofstream& f, uint32_t v) {
  char b[4];
  for (int i = 0; i < 4; ++i) b[i] = static_cast<char>((v >> (8 * i)) & 0xFF);
  f.write(b, 4);
}
void put_u64(std::ofstream& f, uint64_t v) {
  char b[8];
  for (int i = 0; i < 8; ++i) b[i] = static_cast<char>((v >> (8 * i)) & 0xFF);
  f.write(b, 8);
}
bool get_u16(std::ifstream& f, uint16_t* v) {
  unsigned char b[2];
  if (!f.read(reinterpret_cast<char*>(b), 2)) return false;
  *v = static_cast<uint16_t>(b[0] | (b[1] << 8));
  return true;
}
bool get_u32(std::ifstream& f, uint32_t* v) {
  unsigned char b[4];
  if (!f.read(reinterpret_cast<char*>(b), 4)) return false;
  *v = 0;
  for (int i = 0; i < 4; ++i) *v |= static_cast<uint32_t>(b[i]) << (8 * i);
  return true;
}
bool get_u64(std::ifstream& f, uint64_t* v) {
  unsigned char b[8];
  if (!f.read(reinterpret_cast<char*>(b), 8)) return false;
  *v = 0;
  for (int i = 0; i < 8; ++i) *v |= static_cast<uint64_t>(b[i]) << (8 * i);
  return true;
}

uint64_t ticks_to_samples(uint64_t pts, uint32_t rate) {
  return static_cast<uint64_t>(
      std::llround(static_cast<double>(pts) * rate / kTimebaseHz));
}

}  // namespace

Diagnostic write_audio_archive(const std::string& path,
                               const AudioArchive& archive) {
  std::ofstream f(path, std::ios::binary | std::ios::trunc);
  if (!f) return diag(Status::IoError, "cannot open audio output: " + path);
  f.write(kMagic, 8);
  put_u32(f, archive.sample_rate);
  put_u16(f, archive.channels);
  put_u32(f, static_cast<uint32_t>(archive.fragments.size()));
  for (const auto& frag : archive.fragments) {
    put_u64(f, frag.start_pts_ticks);
    put_u32(f, static_cast<uint32_t>(frag.samples_s16.size()));
    for (int16_t s : frag.samples_s16) put_u16(f, static_cast<uint16_t>(s));
  }
  if (!f) return diag(Status::IoError, "audio write failed: " + path);
  return {};
}

Diagnostic read_audio_archive(const std::string& path,
                              AudioArchive* archive) {
  std::ifstream f(path, std::ios::binary);
  if (!f) return diag(Status::UsageError, "cannot open audio: " + path);
  int64_t off = 0;
  char magic[8];
  if (!f.read(magic, 8) || std::memcmp(magic, kMagic, 8) != 0)
    return diag(Status::MediaCorrupt, "bad audio archive magic", off);
  off = 8;
  uint16_t channels = 0;
  uint32_t rate = 0, count = 0;
  if (!get_u32(f, &rate) || !get_u16(f, &channels) || !get_u32(f, &count))
    return diag(Status::MediaCorrupt, "truncated audio header",
                static_cast<int64_t>(f.tellg()));
  if (rate == 0 || channels != 1 || count > 1'000'000)
    return diag(Status::MediaCorrupt, "unsupported audio parameters",
                static_cast<int64_t>(f.tellg()));
  archive->sample_rate = rate;
  archive->channels = static_cast<uint16_t>(channels);
  archive->fragments.clear();
  archive->fragments.reserve(count);
  for (uint32_t i = 0; i < count; ++i) {
    PcmFragment frag;
    uint32_t n = 0;
    const int64_t header_pos = static_cast<int64_t>(f.tellg());
    if (!get_u64(f, &frag.start_pts_ticks) || !get_u32(f, &n))
      return diag(Status::MediaCorrupt, "truncated fragment header",
                  header_pos);
    if (n > kMaxBlockBytes / 2)
      return diag(Status::UsageError, "audio fragment exceeds resource limit",
                  static_cast<int64_t>(f.tellg()));
    frag.samples_s16.resize(n);
    for (uint32_t k = 0; k < n; ++k) {
      uint16_t s = 0;
      const int64_t sample_pos = static_cast<int64_t>(f.tellg());
      if (!get_u16(f, &s))
        return diag(Status::MediaCorrupt, "truncated fragment samples",
                    sample_pos);
      frag.samples_s16[k] = static_cast<int16_t>(s);
    }
    archive->fragments.push_back(std::move(frag));
  }
  return {};
}

AudioSyncResult align_audio(const AudioArchive& archive,
                            uint32_t common_rate,
                            uint64_t timeline_end_pts) {
  AudioSyncResult result;
  result.sample_rate = static_cast<int>(common_rate);
  if (archive.fragments.empty()) return result;

  SwrContext* swr = nullptr;
  AVChannelLayout in_layout;
  AVChannelLayout out_layout;
  av_channel_layout_default(&in_layout, 1);
  av_channel_layout_default(&out_layout, 1);
  int rc = swr_alloc_set_opts2(
      &swr, &out_layout, AV_SAMPLE_FMT_FLTP, common_rate, &in_layout,
      AV_SAMPLE_FMT_S16, static_cast<int>(archive.sample_rate), 0, nullptr);
  if (rc < 0 || swr_init(swr) < 0) {
    if (swr) swr_free(&swr);
    return result;
  }

  uint64_t cursor = 0;  // next expected sample index on the common clock
  double max_err = 0.0;
  int64_t actual_total = 0;

  for (const auto& frag : archive.fragments) {
    const uint8_t* in_planes[1] = {
        reinterpret_cast<const uint8_t*>(frag.samples_s16.data())};
    const int in_count = static_cast<int>(frag.samples_s16.size());
    const int cap =
        static_cast<int>(swr_get_out_samples(swr, in_count)) + 32;
    std::vector<float> out(cap);
    uint8_t* out_planes[1] = {reinterpret_cast<uint8_t*>(out.data())};
    int got = swr_convert(swr, out_planes, cap, in_planes, in_count);
    if (got < 0) continue;
    // Drain samples held in the resampler delay buffer so no tail audio is
    // lost at fragment boundaries.
    int delayed = swr_get_out_samples(swr, 0);
    if (delayed > 0) {
      std::vector<float> tail(static_cast<size_t>(delayed));
      uint8_t* tail_planes[1] = {reinterpret_cast<uint8_t*>(tail.data())};
      const int flushed =
          swr_convert(swr, tail_planes, delayed, nullptr, 0);
      if (flushed > 0) {
        std::copy(tail.begin(), tail.begin() + flushed,
                  std::back_inserter(out));
        got += flushed;
      }
    }
    if (got <= 0) continue;
    out.resize(static_cast<size_t>(got));

    const uint64_t wanted =
        ticks_to_samples(frag.start_pts_ticks, common_rate);
    const int64_t placement_error =
        static_cast<int64_t>(wanted) - static_cast<int64_t>(cursor);
    max_err = std::max(
        max_err, std::abs(placement_error) * 1000.0 / common_rate);

    AudioFragment aligned;
    aligned.seq = static_cast<uint32_t>(result.fragments.size());
    aligned.start_pts_ticks = frag.start_pts_ticks;
    if (wanted > cursor) {
      // Preserve the fragment boundary: insert silence for the gap instead
      // of shifting audio content forward.
      aligned.samples.assign(static_cast<size_t>(wanted - cursor), 0.0f);
    }
    aligned.samples.insert(aligned.samples.end(), out.begin(), out.end());
    result.fragments.push_back(std::move(aligned));

    cursor = wanted + static_cast<uint64_t>(got);
    actual_total += got;
  }
  swr_free(&swr);

  result.max_sync_error_ms = max_err;
  result.actual_samples = actual_total;
  result.expected_samples =
      static_cast<int64_t>(ticks_to_samples(timeline_end_pts, common_rate));
  result.audio_sample_delta = result.actual_samples - result.expected_samples;
  // End-to-end error: how far the audio tail lands from the video tail.
  result.end_to_end_sync_error_ms =
      static_cast<double>(result.audio_sample_delta) * 1000.0 / common_rate;
  return result;
}

}  // namespace media_archive

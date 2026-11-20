#include "silence.hpp"

#include <algorithm>
#include <cmath>
#include <cstring>
#include <map>

extern "C" {
#include <libavutil/channel_layout.h>
#include <libavutil/opt.h>
#include <libswresample/swresample.h>
}

namespace mb {

std::vector<float> pcm16le_to_float(const std::vector<u8>& payload,
                                    u16 channels) {
  std::vector<float> out;
  if (channels == 0) return out;
  std::size_t frames = payload.size() / (std::size_t(channels) * 2u);
  out.resize(frames * std::size_t(channels));
  for (std::size_t i = 0; i < frames * std::size_t(channels); ++i) {
    std::int16_t s = static_cast<std::int16_t>(
        u16(payload[i * 2]) | (u16(payload[i * 2 + 1]) << 8));
    out[i] = static_cast<float>(s) / 32768.0f;
  }
  return out;
}

i64 pcm_sample_duration_ns(std::size_t frame_count, u32 sample_rate) {
  return i64(double(frame_count) * double(kNsPerSec) / double(sample_rate));
}

struct SilenceDetector::Impl {
  u32 sample_rate;
  u16 channels;
  SwrContext* swr = nullptr;

  Impl(u32 rate, u16 ch) : sample_rate(rate), channels(ch) {
    AVChannelLayout in_layout{};
    AVChannelLayout out_layout{};
    av_channel_layout_default(&in_layout, int(ch));
    av_channel_layout_default(&out_layout, int(ch));
    swr_alloc_set_opts2(&swr, &out_layout, AV_SAMPLE_FMT_FLTP, int(rate),
                        &in_layout, AV_SAMPLE_FMT_S16, int(rate), 0, nullptr);
    av_channel_layout_uninit(&in_layout);
    av_channel_layout_uninit(&out_layout);
    if (swr) swr_init(swr);
  }
  ~Impl() {
    if (swr) swr_free(&swr);
  }
};

SilenceDetector::SilenceDetector(u32 sample_rate, u16 channels)
    : impl_(new Impl(sample_rate, channels)) {}

SilenceDetector::~SilenceDetector() { delete impl_; }

SilenceResult SilenceDetector::analyse(
    const std::vector<Segment>& segments,
    const std::vector<PacketRecord>& packets) {
  (void)segments;  // packet timeline already carries segment boundaries
  SilenceResult res;
  res.per_channel_ns.assign(impl_->channels, 0);

  // Per-channel open silence window state.
  struct Win {
    i64 start_ns = 0;
    i64 last_ns = 0;
    double sum_sq = 0.0;
    u64 samples = 0;
    bool open = false;
  };
  std::vector<Win> wins(impl_->channels);

  auto close_window = [&](Win& w, u16 ch) {
    if (!w.open) return;
    i64 dur = w.last_ns - w.start_ns;
    if (dur >= kMinSilenceNs) {
      double rms = w.samples ? std::sqrt(w.sum_sq / double(w.samples)) : 0.0;
      res.ranges.push_back({w.start_ns, w.last_ns, ch, rms});
      res.per_channel_ns[ch] += u64(dur);
    }
    w.open = false;
    w.sum_sq = 0.0;
    w.samples = 0;
  };

  for (const auto& p : packets) {
    if (p.payload.size() < std::size_t(impl_->channels) * 2u) {
      res.decode_errors++;
      for (u16 c = 0; c < impl_->channels; ++c) close_window(wins[c], c);
      continue;
    }
    std::vector<float> interleaved =
        pcm16le_to_float(p.payload, impl_->channels);
    std::size_t frames = interleaved.size() / std::size_t(impl_->channels);
    // FLTP output: one contiguous planar buffer, one pointer per channel.
    std::vector<float> planar_out(interleaved.size(), 0.0f);
    u8* out_planes[32] = {};
    for (u16 c = 0; c < impl_->channels; ++c)
      out_planes[c] = reinterpret_cast<u8*>(planar_out.data() + c * frames);
    u8 const* in_planes[1] = {
        reinterpret_cast<const u8*>(interleaved.data())};
    int got = 0;
    if (impl_->swr) {
      got = swr_convert(impl_->swr, out_planes, int(frames), in_planes,
                        int(frames));
    }
    if (got < 0) {
      res.decode_errors++;
      for (u16 c = 0; c < impl_->channels; ++c) close_window(wins[c], c);
      continue;
    }
    res.window_count++;
    i64 step = pcm_sample_duration_ns(frames, impl_->sample_rate);
    for (u16 c = 0; c < impl_->channels; ++c) {
      double sum_sq = 0.0;
      const float* ch = planar_out.data() + c * frames;
      for (std::size_t f = 0; f < frames; ++f) {
        float v = ch[f];
        sum_sq += double(v) * double(v);
      }
      double rms = frames ? std::sqrt(sum_sq / double(frames)) : 0.0;
      Win& w = wins[c];
      i64 t0 = p.mapped_ns;
      i64 t1 = t0 + step;
      if (rms < kSilenceRmsThreshold) {
        if (!w.open) {
          w.open = true;
          w.start_ns = t0;
        }
        w.last_ns = t1;
        w.sum_sq += sum_sq;
        w.samples += frames;
      } else {
        close_window(w, c);
      }
    }
  }
  for (u16 c = 0; c < impl_->channels; ++c) close_window(wins[c], c);

  // Union duration: merge intervals across channels so overlap counts once.
  std::vector<std::pair<i64, i64>> iv;
  for (const auto& r : res.ranges) iv.emplace_back(r.start_ns, r.end_ns);
  std::sort(iv.begin(), iv.end());
  for (std::size_t i = 0; i < iv.size();) {
    i64 a = iv[i].first, b = iv[i].second;
    std::size_t j = i + 1;
    while (j < iv.size() && iv[j].first <= b) {
      b = std::max(b, iv[j].second);
      ++j;
    }
    res.silence_ns += u64(b - a);
    i = j;
  }
  return res;
}

}  // namespace mb

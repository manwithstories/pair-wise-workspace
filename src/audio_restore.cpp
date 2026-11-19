#include "audio_restore.hpp"

#include <algorithm>
#include <cmath>
#include <cstring>
#include <limits>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/opt.h>
#include <libavutil/samplefmt.h>
#include <libswresample/swresample.h>
}

#include "file_io.hpp"

namespace media_restore {
namespace {

constexpr uint64_t kMaxChannels = 8;
constexpr int kLoudnessRate = 48000;
// EBU R128 / ITU-R BS.1770-4 gating: 400 ms blocks with 75% overlap, an
// absolute gate at -70 LUFS and a relative gate 10 LU below the gated mean.
constexpr double kAbsoluteGateLufs = -70.0;
constexpr int64_t kBlockFrames = kLoudnessRate * 400 / 1000;
constexpr int64_t kBlockStep = kBlockFrames / 4;
constexpr double kSilenceLufs = -100.0;
constexpr double kSilenceThresholdDbfs = -100.0;
constexpr int kTruePeakOversample = 4;

std::string av_error(int error) {
  char buffer[AV_ERROR_MAX_STRING_SIZE] = {};
  av_strerror(error, buffer, sizeof(buffer));
  return std::string(buffer);
}

Diagnostic make(ErrorCode code, std::string message, const Location& location = {}) {
  Diagnostic diagnostic;
  diagnostic.code = code;
  diagnostic.message = std::move(message);
  diagnostic.location = location;
  return diagnostic;
}

bool valid_layout(const AVChannelLayout& layout) {
  return layout.nb_channels > 0 &&
         static_cast<uint64_t>(layout.nb_channels) <= kMaxChannels;
}

void copy_layout(const AVChannelLayout& source, AVChannelLayout* target) {
  av_channel_layout_uninit(target);
  if (av_channel_layout_copy(target, &source) < 0) {
    *target = AVChannelLayout{};
  }
}

// Append `count` planar frames from `scratch` onto `buffer`.  The planar
// stride of PcmBuffer equals its frame count, so growing the buffer changes
// where every existing plane starts: the old data must be repacked into the
// new stride rather than resized in place.
bool append_planes(PcmBuffer* buffer,
                   const std::vector<std::vector<float>>& scratch, int64_t count) {
  if (count <= 0) return true;
  const int64_t old_frames = buffer->frames;
  if (static_cast<__int128>(old_frames + count) * buffer->channels >
      std::numeric_limits<int64_t>::max()) {
    return false;
  }
  const int channels = buffer->channels;
  const int64_t new_frames = old_frames + count;
  std::vector<float> packed(static_cast<size_t>(new_frames) *
                            static_cast<size_t>(channels));
  const float* old_data = buffer->data.data();
  for (int c = 0; c < channels; ++c) {
    float* target = packed.data() + static_cast<size_t>(c) * new_frames;
    if (old_frames > 0) {
      std::memcpy(target, old_data + static_cast<size_t>(c) * old_frames,
                  static_cast<size_t>(old_frames) * sizeof(float));
    }
    std::memcpy(target + old_frames,
                scratch[static_cast<size_t>(c)].data(),
                static_cast<size_t>(count) * sizeof(float));
  }
  buffer->data = std::move(packed);
  buffer->frames = new_frames;
  return true;
}

// Resamples planar float input into planar float output at the destination
// rate/layout, flushing the resampler tail so no samples are lost.
bool resample_planar(const PcmBuffer& input, const AVChannelLayout& layout,
                     int rate, int64_t max_frames, PcmBuffer* out,
                     std::vector<Diagnostic>* diagnostics) {
  const int channels = layout.nb_channels;
  if (input.sample_rate == rate && !input.interleaved &&
      av_channel_layout_compare(&input.layout, &layout) == 0 &&
      input.channels == channels) {
    // Same rate and layout: swresample would still run its format converter
    // and inject an internal delay, shifting every sample.  Copy instead.
    if (static_cast<__int128>(input.frames) * channels >
        static_cast<__int128>(max_frames) * kMaxChannels) {
      diagnostics->push_back(make(ErrorCode::audio_too_large,
                                  "audio exceeds the sample limit"));
      return false;
    }
    PcmBuffer copy;
    copy.sample_rate = rate;
    copy_layout(layout, &copy.layout);
    copy.channels = channels;
    copy.interleaved = false;
    copy.frames = input.frames;
    copy.data = input.data;
    *out = std::move(copy);
    return true;
  }
  const AVSampleFormat input_format =
      input.interleaved ? AV_SAMPLE_FMT_FLT : AV_SAMPLE_FMT_FLTP;
  SwrContext* swr = nullptr;
  int error = swr_alloc_set_opts2(&swr, &layout, AV_SAMPLE_FMT_FLTP, rate,
                                  &input.layout, input_format,
                                  input.sample_rate, 0, nullptr);
  if (error < 0 || swr == nullptr) {
    if (swr != nullptr) swr_free(&swr);
    diagnostics->push_back(make(ErrorCode::audio_decode,
                                "cannot allocate resampler: " + av_error(error)));
    return false;
  }
  if (swr_init(swr) < 0) {
    swr_free(&swr);
    diagnostics->push_back(make(ErrorCode::audio_decode,
                                "cannot initialise resampler"));
    return false;
  }

  PcmBuffer converted;
  converted.sample_rate = rate;
  copy_layout(layout, &converted.layout);
  converted.channels = channels;
  converted.interleaved = false;

  const auto drain = [&](const uint8_t* const* planes, int count) -> bool {
    const int capacity = swr_get_out_samples(swr, count);
    if (capacity < 0) return false;
    if (capacity == 0) return true;
    if (static_cast<__int128>(converted.frames + capacity) * channels >
        static_cast<__int128>(max_frames) * kMaxChannels) {
      diagnostics->push_back(make(ErrorCode::audio_too_large,
                                  "resampled audio exceeds the sample limit"));
      return false;
    }
    std::vector<std::vector<float>> scratch(
        static_cast<size_t>(channels),
        std::vector<float>(static_cast<size_t>(capacity), 0.0f));
    std::vector<uint8_t*> targets(static_cast<size_t>(channels));
    for (int c = 0; c < channels; ++c) {
      targets[static_cast<size_t>(c)] =
          reinterpret_cast<uint8_t*>(scratch[static_cast<size_t>(c)].data());
    }
    const int produced = swr_convert(swr, targets.data(), capacity, planes, count);
    if (produced < 0) {
      diagnostics->push_back(make(ErrorCode::audio_decode,
                                  "resampler failed: " + av_error(produced)));
      return false;
    }
    return append_planes(&converted, scratch, produced);
  };

  const uint8_t* input_planes[8] = {};
  if (input.interleaved) {
    input_planes[0] = reinterpret_cast<const uint8_t*>(input.data.data());
  } else {
    for (int c = 0; c < input.channels && c < 8; ++c) {
      input_planes[c] = reinterpret_cast<const uint8_t*>(input.plane(c));
    }
  }
  bool ok = drain(input_planes, static_cast<int>(input.frames));
  if (ok) {
    // Flush the resampler's internal delay.  swr_get_out_samples(swr, 0)
    // reports the pending tail; the loop is bounded by a zero return so a
    // resampler that keeps advertising a delay cannot spin forever.
    constexpr int kMaxFlushPasses = 64;
    for (int pass = 0; ok && pass < kMaxFlushPasses; ++pass) {
      const int pending = swr_get_out_samples(swr, 0);
      if (pending <= 0) break;
      std::vector<std::vector<float>> scratch(
          static_cast<size_t>(channels),
          std::vector<float>(static_cast<size_t>(pending), 0.0f));
      std::vector<uint8_t*> targets(static_cast<size_t>(channels));
      for (int c = 0; c < channels; ++c) {
        targets[static_cast<size_t>(c)] =
            reinterpret_cast<uint8_t*>(scratch[static_cast<size_t>(c)].data());
      }
      const int flushed = swr_convert(swr, targets.data(), pending, nullptr, 0);
      if (flushed < 0) {
        ok = false;
        break;
      }
      if (flushed == 0) break;
      ok = append_planes(&converted, scratch, flushed);
    }
  }
  swr_free(&swr);
  if (!ok) return false;
  *out = std::move(converted);
  return true;
}

// BS.1770 channel weights: L/R/C = 1.0, rear/side = 1.41, LFE = 0.
double channel_weight(const AVChannelLayout& layout, int channel) {
  if (layout.order != AV_CHANNEL_ORDER_NATIVE) return 1.0;
  const uint64_t kLfe = 1ULL << AV_CHAN_LOW_FREQUENCY;
  const uint64_t kRear = (1ULL << AV_CHAN_BACK_LEFT) |
                         (1ULL << AV_CHAN_BACK_RIGHT) |
                         (1ULL << AV_CHAN_SIDE_LEFT) |
                         (1ULL << AV_CHAN_SIDE_RIGHT);
  const AVChannel order[8] = {AV_CHAN_FRONT_LEFT,  AV_CHAN_FRONT_RIGHT,
                              AV_CHAN_FRONT_CENTER, AV_CHAN_LOW_FREQUENCY,
                              AV_CHAN_BACK_LEFT,  AV_CHAN_BACK_RIGHT,
                              AV_CHAN_SIDE_LEFT,  AV_CHAN_SIDE_RIGHT};
  if (channel < 0 || channel >= 8) return 1.0;
  const uint64_t bit = 1ULL << order[channel];
  if (bit & kLfe) return 0.0;
  if (bit & kRear) return 1.41;
  return 1.0;
}

// K-weighting biquads at 48 kHz (BS.1770-4 tables 1 and 2).
struct Biquad {
  double b0, b1, b2, a1, a2;
  double z1 = 0.0, z2 = 0.0;
  double process(double x) {
    const double y = b0 * x + z1;
    z1 = b1 * x - a1 * y + z2;
    z2 = b2 * x - a2 * y;
    return y;
  }
};

Biquad shelf_filter() {
  return Biquad{1.53512485958697, -2.69169618940638, 1.19839281085285,
                -1.69065929318241, 0.73248077421585};
}

Biquad high_pass_filter() {
  return Biquad{1.0, -2.0, 1.0, -1.99004745483398, 0.99007225036621};
}

// True peak: 4x linear interpolation of the sample peak.  Deterministic and
// adequate for archive limiting; not a substitute for a broadcast meter.
double true_peak_dbfs(const PcmBuffer& buffer) {
  if (buffer.frames <= 0 || buffer.channels <= 0) return kSilenceLufs;
  double peak = 0.0;
  for (int c = 0; c < buffer.channels; ++c) {
    const float* samples = buffer.plane(c);
    for (int64_t i = 0; i < buffer.frames; ++i) {
      peak = std::max(peak, std::fabs(static_cast<double>(samples[i])));
      if (i > 0) {
        const double a = samples[i - 1];
        const double b = samples[i];
        for (int k = 1; k < kTruePeakOversample; ++k) {
          const double t = static_cast<double>(k) / kTruePeakOversample;
          peak = std::max(peak, std::fabs(a + (b - a) * t));
        }
      }
    }
  }
  if (peak <= 0.0) return kSilenceLufs;
  return std::max(20.0 * std::log10(peak), kSilenceLufs);
}

double sample_peak_dbfs(const PcmBuffer& buffer) {
  double peak = 0.0;
  for (int c = 0; c < buffer.channels; ++c) {
    const float* samples = buffer.plane(c);
    for (int64_t i = 0; i < buffer.frames; ++i) {
      peak = std::max(peak, std::fabs(static_cast<double>(samples[i])));
    }
  }
  if (peak <= 0.0) return kSilenceLufs;
  return std::max(20.0 * std::log10(peak), kSilenceLufs);
}

}  // namespace

PcmBuffer::PcmBuffer() = default;

PcmBuffer::PcmBuffer(const PcmBuffer& other)
    : sample_rate(other.sample_rate),
      interleaved(other.interleaved),
      channels(other.channels),
      frames(other.frames),
      data(other.data) {
  copy_layout(other.layout, &layout);
}

PcmBuffer& PcmBuffer::operator=(const PcmBuffer& other) {
  if (this == &other) return *this;
  sample_rate = other.sample_rate;
  interleaved = other.interleaved;
  channels = other.channels;
  frames = other.frames;
  data = other.data;
  copy_layout(other.layout, &layout);
  return *this;
}

PcmBuffer::PcmBuffer(PcmBuffer&& other) noexcept
    : sample_rate(other.sample_rate),
      interleaved(other.interleaved),
      channels(other.channels),
      frames(other.frames),
      data(std::move(other.data)) {
  layout = other.layout;
  other.layout = AVChannelLayout{};
  other.frames = 0;
  other.channels = 0;
}

PcmBuffer& PcmBuffer::operator=(PcmBuffer&& other) noexcept {
  if (this == &other) return *this;
  av_channel_layout_uninit(&layout);
  sample_rate = other.sample_rate;
  interleaved = other.interleaved;
  channels = other.channels;
  frames = other.frames;
  data = std::move(other.data);
  layout = other.layout;
  other.layout = AVChannelLayout{};
  other.frames = 0;
  other.channels = 0;
  return *this;
}

PcmBuffer::~PcmBuffer() { av_channel_layout_uninit(&layout); }

float* PcmBuffer::plane(int channel) {
  if (channel < 0 || channel >= channels) return nullptr;
  if (interleaved) return data.data();
  return data.data() + static_cast<size_t>(channel) * static_cast<size_t>(frames);
}

float* PcmBuffer::frame(int64_t index) {
  if (!interleaved || index < 0 || index >= frames) return nullptr;
  return data.data() + static_cast<size_t>(index) * static_cast<size_t>(channels);
}

const float* PcmBuffer::plane(int channel) const {
  return const_cast<PcmBuffer*>(this)->plane(channel);
}

const float* PcmBuffer::frame(int64_t index) const {
  return const_cast<PcmBuffer*>(this)->frame(index);
}

AudioRestoreConfig::AudioRestoreConfig() {
  av_channel_layout_default(&output_layout, 2);
}

AudioRestoreConfig::AudioRestoreConfig(AudioRestoreConfig&& other) noexcept
    : output_rate(other.output_rate),
      target_lufs(other.target_lufs),
      true_peak_dbtp(other.true_peak_dbtp),
      apply_gain(other.apply_gain),
      max_output_frames(other.max_output_frames) {
  output_layout = other.output_layout;
  other.output_layout = AVChannelLayout{};
}

AudioRestoreConfig& AudioRestoreConfig::operator=(AudioRestoreConfig&& other) noexcept {
  if (this == &other) return *this;
  av_channel_layout_uninit(&output_layout);
  output_rate = other.output_rate;
  target_lufs = other.target_lufs;
  true_peak_dbtp = other.true_peak_dbtp;
  apply_gain = other.apply_gain;
  max_output_frames = other.max_output_frames;
  output_layout = other.output_layout;
  other.output_layout = AVChannelLayout{};
  return *this;
}

AudioRestoreConfig::~AudioRestoreConfig() { av_channel_layout_uninit(&output_layout); }

bool parse_layout(std::string_view name, AVChannelLayout* out) {
  if (name.empty() || name.size() > 32) return false;
  const std::string text(name);
  AVChannelLayout layout{};
  if (av_channel_layout_from_string(&layout, text.c_str()) < 0) return false;
  // Only named, canonical layouts.  A bare channel count like "6" is
  // ambiguous and must not silently become a mask.
  const bool named = av_channel_layout_check(&layout) &&
                     layout.nb_channels > 0 &&
                     static_cast<uint64_t>(layout.nb_channels) <= kMaxChannels &&
                     layout.order == AV_CHANNEL_ORDER_NATIVE;
  if (!named) {
    av_channel_layout_uninit(&layout);
    return false;
  }
  av_channel_layout_uninit(out);
  *out = layout;
  return true;
}

std::string layout_name(const AVChannelLayout& layout) {
  if (!valid_layout(layout)) return "unknown";
  char buffer[64];
  if (av_channel_layout_describe(&layout, buffer, sizeof(buffer)) < 0) {
    return "unknown";
  }
  return std::string(buffer);
}

bool convert_audio(const PcmBuffer& input, const AudioRestoreConfig& config,
                   PcmBuffer* out, AudioStats* stats,
                   std::vector<Diagnostic>* diagnostics) {
  if (!valid_layout(input.layout) || input.channels <= 0) {
    diagnostics->push_back(make(ErrorCode::audio_unsupported_layout,
                                "input has no usable channel layout"));
    return false;
  }
  if (!valid_layout(config.output_layout)) {
    diagnostics->push_back(make(ErrorCode::audio_unsupported_layout,
                                "target layout is not supported"));
    return false;
  }
  if (input.sample_rate <= 0 || config.output_rate <= 0 ||
      config.output_rate > 768000) {
    diagnostics->push_back(make(ErrorCode::audio_unsupported_rate,
                                "sample rate must be in (0, 768000]"));
    return false;
  }
  if (input.frames < 0 ||
      static_cast<__int128>(input.frames) * input.channels >
          static_cast<__int128>(config.max_output_frames) * kMaxChannels) {
    diagnostics->push_back(make(ErrorCode::audio_too_large,
                                "input exceeds the audio sample limit"));
    return false;
  }

  stats->input_rate = input.sample_rate;
  stats->output_rate = config.output_rate;
  stats->input_layout = layout_name(input.layout);
  stats->output_layout = layout_name(config.output_layout);
  stats->input_frames = input.frames;

  if (!resample_planar(input, config.output_layout, config.output_rate,
                       config.max_output_frames, out, diagnostics)) {
    return false;
  }
  stats->output_frames = out->frames;
  return true;
}

bool apply_timeline_offset(int64_t offset_samples, PcmBuffer* buffer,
                           AudioStats* stats) {
  if (offset_samples == 0) return true;
  if (offset_samples > 0) {
    // Positive offset: the audio starts later than the segment, so prepend
    // silence at the destination rate.
    const int64_t frames = offset_samples;
    if (static_cast<__int128>(buffer->frames + frames) * buffer->channels >
        std::numeric_limits<int64_t>::max()) {
      return false;
    }
    const int64_t source_frames = buffer->frames;
    std::vector<std::vector<float>> scratch(
        static_cast<size_t>(buffer->channels),
        std::vector<float>(static_cast<size_t>(frames + source_frames), 0.0f));
    for (int c = 0; c < buffer->channels; ++c) {
      std::memcpy(scratch[static_cast<size_t>(c)].data() + frames,
                  buffer->plane(c),
                  static_cast<size_t>(source_frames) * sizeof(float));
    }
    PcmBuffer shifted;
    shifted.sample_rate = buffer->sample_rate;
    copy_layout(buffer->layout, &shifted.layout);
    shifted.channels = buffer->channels;
    shifted.interleaved = false;
    shifted.frames = frames + source_frames;
    shifted.data.assign(static_cast<size_t>(shifted.frames) * shifted.channels,
                        0.0f);
    for (int c = 0; c < shifted.channels; ++c) {
      std::memcpy(shifted.plane(c), scratch[static_cast<size_t>(c)].data(),
                  static_cast<size_t>(shifted.frames) * sizeof(float));
    }
    *buffer = std::move(shifted);
    stats->padded_frames = frames;
    return true;
  }
  // Negative offset: the source starts before the segment, so trim leading
  // samples until the first output sample aligns with the segment start.
  const int64_t drop = -offset_samples;
  if (drop >= buffer->frames) {
    stats->discarded_frames = buffer->frames;
    buffer->frames = 0;
    buffer->data.clear();
    return true;
  }
  for (int c = 0; c < buffer->channels; ++c) {
    std::memmove(buffer->plane(c), buffer->plane(c) + drop,
                 static_cast<size_t>(buffer->frames - drop) * sizeof(float));
  }
  buffer->frames -= drop;
  buffer->data.resize(static_cast<size_t>(buffer->frames) * buffer->channels);
  stats->discarded_frames = drop;
  return true;
}

bool measure_loudness(const PcmBuffer& buffer, double* lufs, double* true_peak,
                      bool* silent, std::vector<Diagnostic>* diagnostics) {
  *lufs = kSilenceLufs;
  *true_peak = kSilenceLufs;
  *silent = true;
  if (buffer.channels <= 0 || buffer.frames < 0) {
    diagnostics->push_back(make(ErrorCode::loudness_invalid,
                                "cannot measure a buffer without channels"));
    return false;
  }
  if (buffer.frames == 0) return true;  // empty segment: silence floor
  *true_peak = true_peak_dbfs(buffer);
  if (sample_peak_dbfs(buffer) < kSilenceThresholdDbfs) return true;
  *silent = false;

  // Measurement always happens at 48 kHz because the K-weighting coefficients
  // are defined there; the delivery rate may be anything.
  PcmBuffer measured;
  if (buffer.sample_rate != kLoudnessRate) {
    if (!resample_planar(buffer, buffer.layout, kLoudnessRate,
                         buffer.frames * 4 + kLoudnessRate, &measured,
                         diagnostics)) {
      return false;
    }
  } else {
    measured = buffer;
  }
  if (measured.frames < kBlockStep) {
    diagnostics->push_back(make(
        ErrorCode::loudness_invalid,
        "segment is shorter than one 100 ms gating block; loudness stays at "
        "the silence floor",
        {}));
    return true;
  }

  // Filter every channel with continuous state, then form gating blocks.
  const int channels = measured.channels;
  std::vector<std::vector<double>> filtered(
      static_cast<size_t>(channels),
      std::vector<double>(static_cast<size_t>(measured.frames)));
  for (int c = 0; c < channels; ++c) {
    Biquad shelf = shelf_filter();
    Biquad high_pass = high_pass_filter();
    const float* samples = measured.plane(c);
    for (int64_t i = 0; i < measured.frames; ++i) {
      filtered[static_cast<size_t>(c)][static_cast<size_t>(i)] =
          high_pass.process(shelf.process(static_cast<double>(samples[i])));
    }
  }
  std::vector<double> block_levels;
  for (int64_t start = 0; start + kBlockFrames <= measured.frames;
       start += kBlockStep) {
    double sum = 0.0;
    for (int c = 0; c < channels; ++c) {
      double energy = 0.0;
      const auto& channel = filtered[static_cast<size_t>(c)];
      for (int64_t i = start; i < start + kBlockFrames; ++i) {
        energy += channel[static_cast<size_t>(i)] * channel[static_cast<size_t>(i)];
      }
      energy /= static_cast<double>(kBlockFrames);
      sum += channel_weight(measured.layout, c) * energy;
    }
    block_levels.push_back(-0.691 + 10.0 * std::log10(sum + 1e-30));
  }
  if (block_levels.empty()) return true;

  std::vector<double> absolute_gated;
  for (const double level : block_levels) {
    if (level > kAbsoluteGateLufs) absolute_gated.push_back(level);
  }
  if (absolute_gated.empty()) {
    *silent = true;
    return true;
  }
  double sum = 0.0;
  for (const double level : absolute_gated) sum += std::pow(10.0, level / 10.0);
  const double relative_gate =
      -0.691 + 10.0 * std::log10(sum / static_cast<double>(absolute_gated.size())) -
      10.0;
  double final_sum = 0.0;
  size_t final_count = 0;
  for (const double level : block_levels) {
    if (level > std::max(kAbsoluteGateLufs, relative_gate)) {
      final_sum += std::pow(10.0, level / 10.0);
      ++final_count;
    }
  }
  if (final_count == 0) {
    *silent = true;
    return true;
  }
  *lufs = -0.691 + 10.0 * std::log10(final_sum / static_cast<double>(final_count));
  return true;
}

bool normalize_loudness(const AudioRestoreConfig& config, PcmBuffer* buffer,
                        AudioStats* stats,
                        std::vector<Diagnostic>* diagnostics) {
  if (buffer->frames == 0) {
    stats->silent = true;
    stats->loudness_measured = true;
    stats->input_lufs = kSilenceLufs;
    stats->output_lufs = kSilenceLufs;
    return true;
  }
  double lufs = 0.0;
  double true_peak = 0.0;
  bool silent = false;
  if (!measure_loudness(*buffer, &lufs, &true_peak, &silent, diagnostics)) {
    return false;
  }
  stats->input_lufs = lufs;
  stats->input_true_peak_dbtp = true_peak;
  stats->silent = silent;
  stats->loudness_measured = true;
  stats->short_segment = buffer->frames < kBlockStep;
  if (silent) {
    // Silence has no measurable level; never amplify it into noise.
    stats->output_lufs = kSilenceLufs;
    stats->output_true_peak_dbtp = true_peak;
    return true;
  }
  // A segment shorter than one gating block has no valid integrated level;
  // gain is skipped rather than derived from an unmeasurable region.
  if (!config.apply_gain || stats->short_segment) {
    stats->output_lufs = lufs;
    stats->output_true_peak_dbtp = true_peak;
    return true;
  }

  double gain_db = config.target_lufs - lufs;
  constexpr double kMaxGainDb = 30.0;
  gain_db = std::clamp(gain_db, -kMaxGainDb, kMaxGainDb);
  const double gain = std::pow(10.0, gain_db / 20.0);
  for (int c = 0; c < buffer->channels; ++c) {
    float* samples = buffer->plane(c);
    for (int64_t i = 0; i < buffer->frames; ++i) {
      samples[i] = static_cast<float>(samples[i] * gain);
    }
  }
  stats->applied_gain_db = gain_db;

  const double ceiling = std::pow(10.0, config.true_peak_dbtp / 20.0);
  double peak = 0.0;
  for (int c = 0; c < buffer->channels; ++c) {
    const float* samples = buffer->plane(c);
    for (int64_t i = 0; i < buffer->frames; ++i) {
      peak = std::max(peak, std::fabs(static_cast<double>(samples[i])));
    }
  }
  if (peak > ceiling && peak > 0.0) {
    const double scale = ceiling / peak;
    for (int c = 0; c < buffer->channels; ++c) {
      float* samples = buffer->plane(c);
      for (int64_t i = 0; i < buffer->frames; ++i) {
        const double value = samples[i] * scale;
        if (std::fabs(value) >= ceiling) ++stats->limited_samples;
        samples[i] = static_cast<float>(std::clamp(value, -ceiling, ceiling));
      }
    }
    stats->peak_limited = true;
    stats->applied_gain_db += 20.0 * std::log10(scale);
  }
  double out_lufs = 0.0;
  double out_peak = 0.0;
  bool out_silent = false;
  if (!measure_loudness(*buffer, &out_lufs, &out_peak, &out_silent, diagnostics)) {
    return false;
  }
  stats->output_lufs = out_lufs;
  stats->output_true_peak_dbtp = out_peak;
  return true;
}

namespace {

void put_u32(std::string* out, uint32_t value) {
  out->push_back(static_cast<char>(value & 0xFF));
  out->push_back(static_cast<char>((value >> 8) & 0xFF));
  out->push_back(static_cast<char>((value >> 16) & 0xFF));
  out->push_back(static_cast<char>((value >> 24) & 0xFF));
}

void put_u16(std::string* out, uint16_t value) {
  out->push_back(static_cast<char>(value & 0xFF));
  out->push_back(static_cast<char>((value >> 8) & 0xFF));
}

}  // namespace

bool encode_wav(const PcmBuffer& buffer, std::string* out,
                std::vector<Diagnostic>* diagnostics) {
  if (buffer.channels <= 0 || buffer.sample_rate <= 0 || buffer.frames < 0) {
    diagnostics->push_back(make(ErrorCode::audio_decode,
                                "cannot encode a WAV without format information"));
    return false;
  }
  const __int128 data_bytes =
      static_cast<__int128>(buffer.frames) * buffer.channels * 4;
  if (data_bytes > 0x7FFFFFFF) {
    diagnostics->push_back(make(ErrorCode::audio_too_large,
                                "WAV payload exceeds the RIFF 32-bit size field"));
    return false;
  }
  // WAVE_FORMAT_EXTENSIBLE carries the channel mask, so 5.1/7.1 survive a
  // round trip through any decoder instead of degrading to a channel count.
  const bool extensible =
      buffer.channels > 2 && buffer.layout.order == AV_CHANNEL_ORDER_NATIVE;
  const uint32_t fmt_size = extensible ? 40 : 16;
  std::string& wav = *out;
  wav.clear();
  wav.reserve(static_cast<size_t>(data_bytes) + 68);
  wav.append("RIFF", 4);
  put_u32(&wav, static_cast<uint32_t>(36 + (fmt_size - 16) + data_bytes));
  wav.append("WAVE", 4);
  wav.append("fmt ", 4);
  put_u32(&wav, fmt_size);
  put_u16(&wav, extensible ? 0xFFFE : 3);
  put_u16(&wav, static_cast<uint16_t>(buffer.channels));
  put_u32(&wav, static_cast<uint32_t>(buffer.sample_rate));
  const uint32_t block_align = static_cast<uint32_t>(buffer.channels) * 4;
  put_u32(&wav, static_cast<uint32_t>(buffer.sample_rate) * block_align);
  put_u16(&wav, static_cast<uint16_t>(block_align));
  put_u16(&wav, 32);
  if (extensible) {
    put_u16(&wav, 22);
    put_u16(&wav, 32);
    put_u32(&wav, static_cast<uint32_t>(buffer.layout.u.mask));
    const unsigned char guid[16] = {0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10,
                                    0x00, 0x80, 0x00, 0x00, 0xAA, 0x00, 0x38,
                                    0x9B, 0x71};
    wav.append(reinterpret_cast<const char*>(guid), sizeof(guid));
  }
  wav.append("data", 4);
  put_u32(&wav, static_cast<uint32_t>(data_bytes));
  // Interleave in channel order.  Little-endian float bit patterns are stable
  // across Apple Silicon and Intel, so the hash does not depend on the host.
  for (int64_t i = 0; i < buffer.frames; ++i) {
    for (int c = 0; c < buffer.channels; ++c) {
      const float value = buffer.plane(c)[i];
      uint32_t bits = 0;
      std::memcpy(&bits, &value, sizeof(bits));
      put_u32(&wav, bits);
    }
  }
  return true;
}

bool decode_audio_file(const std::filesystem::path& path, const Limits& limits,
                       PcmBuffer* out, std::vector<Diagnostic>* diagnostics) {
  AVFormatContext* format = nullptr;
  int error = avformat_open_input(&format, path.c_str(), nullptr, nullptr);
  if (error < 0) {
    diagnostics->push_back(make(ErrorCode::io,
                                "cannot open audio: " + av_error(error),
                                {.file = path.string()}));
    return false;
  }
  error = avformat_find_stream_info(format, nullptr);
  if (error < 0) {
    avformat_close_input(&format);
    diagnostics->push_back(make(ErrorCode::audio_decode,
                                "cannot read audio stream info: " + av_error(error),
                                {.file = path.string()}));
    return false;
  }
  const int stream_index =
      av_find_best_stream(format, AVMEDIA_TYPE_AUDIO, -1, -1, nullptr, 0);
  if (stream_index < 0) {
    avformat_close_input(&format);
    diagnostics->push_back(make(ErrorCode::audio_decode,
                                "file has no audio stream", {.file = path.string()}));
    return false;
  }
  AVStream* stream = format->streams[stream_index];
  const AVCodec* decoder = avcodec_find_decoder(stream->codecpar->codec_id);
  AVCodecContext* codec = decoder != nullptr ? avcodec_alloc_context3(decoder) : nullptr;
  if (codec == nullptr ||
      avcodec_parameters_to_context(codec, stream->codecpar) < 0 ||
      avcodec_open2(codec, decoder, nullptr) < 0) {
    if (codec != nullptr) avcodec_free_context(&codec);
    avformat_close_input(&format);
    diagnostics->push_back(make(ErrorCode::audio_decode,
                                "cannot open the audio decoder",
                                {.file = path.string()}));
    return false;
  }

  PcmBuffer result;
  result.sample_rate = codec->sample_rate;
  copy_layout(codec->ch_layout, &result.layout);
  result.channels = result.layout.nb_channels;
  result.interleaved = false;
  if (result.channels <= 0 || !valid_layout(result.layout)) {
    avcodec_free_context(&codec);
    avformat_close_input(&format);
    diagnostics->push_back(make(ErrorCode::audio_unsupported_layout,
                                "decoded audio has no supported channel layout",
                                {.file = path.string()}));
    return false;
  }

  // Convert into the decoder's native rate/layout so the rest of the pipeline
  // has one sample representation.  Same-rate, same-layout data skips
  // swresample entirely: its format-conversion path adds an internal delay
  // that would silently shift every decoded sample.
  if (codec->sample_rate == result.sample_rate &&
      (codec->sample_fmt == AV_SAMPLE_FMT_FLTP ||
       codec->sample_fmt == AV_SAMPLE_FMT_FLT)) {
    AVPacket* plain_packet = av_packet_alloc();
    AVFrame* plain_frame = av_frame_alloc();
    bool ok = plain_packet != nullptr && plain_frame != nullptr;
    while (ok && av_read_frame(format, plain_packet) >= 0) {
      if (plain_packet->stream_index != stream_index) {
        av_packet_unref(plain_packet);
        continue;
      }
      if (avcodec_send_packet(codec, plain_packet) < 0) {
        av_packet_unref(plain_packet);
        continue;
      }
      av_packet_unref(plain_packet);
      while (true) {
        const int received = avcodec_receive_frame(codec, plain_frame);
        if (received == AVERROR(EAGAIN) || received == AVERROR_EOF) break;
        if (received < 0) {
          ok = false;
          break;
        }
        std::vector<std::vector<float>> scratch(
            static_cast<size_t>(result.channels),
            std::vector<float>(static_cast<size_t>(plain_frame->nb_samples)));
        const bool planar = codec->sample_fmt == AV_SAMPLE_FMT_FLTP;
        for (int c = 0; c < result.channels; ++c) {
          if (planar) {
            std::memcpy(scratch[static_cast<size_t>(c)].data(),
                        plain_frame->extended_data[c],
                        static_cast<size_t>(plain_frame->nb_samples) *
                            sizeof(float));
          } else {
            const float* source = reinterpret_cast<const float*>(plain_frame->data[0]);
            for (int i = 0; i < plain_frame->nb_samples; ++i) {
              scratch[static_cast<size_t>(c)][static_cast<size_t>(i)] =
                  source[static_cast<size_t>(i) * result.channels + c];
            }
          }
        }
        if (static_cast<__int128>(result.frames + plain_frame->nb_samples) *
                result.channels >
            static_cast<__int128>(limits.max_audio_samples) * kMaxChannels) {
          diagnostics->push_back(make(ErrorCode::audio_too_large,
                                      "decoded audio exceeds the sample limit",
                                      {.file = path.string()}));
          ok = false;
        } else {
          static int dumped = 0;
          if (dumped < 1 && scratch.size() > 1 && scratch[1].size() > 49) {
            ++dumped;
          }
          ok = append_planes(&result, scratch, plain_frame->nb_samples);
        }
        av_frame_unref(plain_frame);
        if (!ok) break;
      }
    }
    if (ok) {
      avcodec_send_packet(codec, nullptr);
      while (true) {
        const int received = avcodec_receive_frame(codec, plain_frame);
        if (received < 0) break;
        std::vector<std::vector<float>> scratch(
            static_cast<size_t>(result.channels),
            std::vector<float>(static_cast<size_t>(plain_frame->nb_samples)));
        for (int c = 0; c < result.channels; ++c) {
          if (codec->sample_fmt == AV_SAMPLE_FMT_FLTP) {
            std::memcpy(scratch[static_cast<size_t>(c)].data(),
                        plain_frame->extended_data[c],
                        static_cast<size_t>(plain_frame->nb_samples) *
                            sizeof(float));
          } else {
            const float* source = reinterpret_cast<const float*>(plain_frame->data[0]);
            for (int i = 0; i < plain_frame->nb_samples; ++i) {
              scratch[static_cast<size_t>(c)][static_cast<size_t>(i)] =
                  source[static_cast<size_t>(i) * result.channels + c];
            }
          }
        }
        ok = append_planes(&result, scratch, plain_frame->nb_samples);
        av_frame_unref(plain_frame);
        if (!ok) break;
      }
    }
    av_packet_free(&plain_packet);
    av_frame_free(&plain_frame);
    avcodec_free_context(&codec);
    avformat_close_input(&format);
    if (!ok) return false;
    *out = std::move(result);
    return true;
  }

  SwrContext* swr = nullptr;
  AVChannelLayout layout{};
  av_channel_layout_copy(&layout, &result.layout);
  error = swr_alloc_set_opts2(
      &swr, &layout, AV_SAMPLE_FMT_FLTP, result.sample_rate, &layout,
      static_cast<AVSampleFormat>(codec->sample_fmt), result.sample_rate, 0,
      nullptr);
  const bool setup = error >= 0 && swr != nullptr && swr_init(swr) >= 0;
  av_channel_layout_uninit(&layout);
  AVPacket* packet = av_packet_alloc();
  AVFrame* frame = av_frame_alloc();
  bool ok = setup && packet != nullptr && frame != nullptr;

  const auto append_decoded = [&](const AVFrame& source) -> bool {
    const int capacity = swr_get_out_samples(swr, source.nb_samples);
    if (capacity <= 0) return true;
    if (static_cast<__int128>(result.frames + capacity) * result.channels >
        static_cast<__int128>(limits.max_audio_samples) * kMaxChannels) {
      diagnostics->push_back(make(ErrorCode::audio_too_large,
                                  "decoded audio exceeds the sample limit",
                                  {.file = path.string()}));
      return false;
    }
    std::vector<std::vector<float>> scratch(
        static_cast<size_t>(result.channels),
        std::vector<float>(static_cast<size_t>(capacity), 0.0f));
    std::vector<uint8_t*> planes(static_cast<size_t>(result.channels));
    for (int c = 0; c < result.channels; ++c) {
      planes[static_cast<size_t>(c)] =
          reinterpret_cast<uint8_t*>(scratch[static_cast<size_t>(c)].data());
    }
    const int produced = swr_convert(
        swr, planes.data(), capacity,
        const_cast<const uint8_t**>(source.extended_data), source.nb_samples);
    if (produced < 0) {
      diagnostics->push_back(make(ErrorCode::audio_decode,
                                  "cannot convert decoded samples: " +
                                      av_error(produced),
                                  {.file = path.string()}));
      return false;
    }
    return append_planes(&result, scratch, produced);
  };

  while (ok && av_read_frame(format, packet) >= 0) {
    if (packet->stream_index != stream_index) {
      av_packet_unref(packet);
      continue;
    }
    if (avcodec_send_packet(codec, packet) < 0) {
      av_packet_unref(packet);
      continue;
    }
    av_packet_unref(packet);
    while (true) {
      const int received = avcodec_receive_frame(codec, frame);
      if (received == AVERROR(EAGAIN) || received == AVERROR_EOF) break;
      if (received < 0) {
        ok = false;
        break;
      }
      ok = append_decoded(*frame);
      av_frame_unref(frame);
      if (!ok) break;
    }
  }
  if (ok) {
    avcodec_send_packet(codec, nullptr);
    while (true) {
      const int received = avcodec_receive_frame(codec, frame);
      if (received < 0) break;
      ok = append_decoded(*frame);
      av_frame_unref(frame);
      if (!ok) break;
    }
  }
  if (ok) {
    const int capacity = swr_get_out_samples(swr, 0);
    if (capacity > 0) {
      std::vector<std::vector<float>> scratch(
          static_cast<size_t>(result.channels),
          std::vector<float>(static_cast<size_t>(capacity), 0.0f));
      std::vector<uint8_t*> planes(static_cast<size_t>(result.channels));
      for (int c = 0; c < result.channels; ++c) {
        planes[static_cast<size_t>(c)] =
            reinterpret_cast<uint8_t*>(scratch[static_cast<size_t>(c)].data());
      }
      const int flushed = swr_convert(swr, planes.data(), capacity, nullptr, 0);
      ok = flushed >= 0 && append_planes(&result, scratch, flushed);
    }
  }
  if (swr != nullptr) swr_free(&swr);
  av_packet_free(&packet);
  av_frame_free(&frame);
  avcodec_free_context(&codec);
  avformat_close_input(&format);
  if (!ok) return false;
  *out = std::move(result);
  return true;
}

bool restore_segment_audio(const std::filesystem::path& source,
                           int64_t offset_ticks, const Rational& time_base,
                           const AudioRestoreConfig& config,
                           RestoredAudio* out,
                           std::vector<Diagnostic>* diagnostics) {
  Limits limits;
  limits.max_audio_samples = config.max_output_frames;
  PcmBuffer decoded;
  if (!decode_audio_file(source, limits, &decoded, diagnostics)) return false;
  const size_t before = diagnostics->size();
  AudioStats stats;
  PcmBuffer converted;
  if (!convert_audio(decoded, config, &converted, &stats, diagnostics)) {
    return false;
  }
  if (offset_ticks != 0) {
    const Rational scaled(offset_ticks * time_base.num, time_base.den);
    int64_t offset_samples = 0;
    if (!Rational::sample_count(scaled, config.output_rate, &offset_samples)) {
      diagnostics->push_back(make(
          ErrorCode::audio_decode,
          "audio offset is not representable at the output sample rate"));
      return false;
    }
    if (!apply_timeline_offset(offset_samples, &converted, &stats)) {
      diagnostics->push_back(make(ErrorCode::audio_too_large,
                                  "offset would overflow the audio buffer"));
      return false;
    }
  }
  if (!normalize_loudness(config, &converted, &stats, diagnostics)) return false;
  if (!encode_wav(converted, &out->wav, diagnostics)) return false;
  out->stats = stats;
  return diagnostics->size() == before;
}

}  // namespace media_restore

#include "audio_restore.hpp"

#include <algorithm>
#include <cmath>
#include <cstring>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/opt.h>
#include <libavutil/samplefmt.h>
#include <libswresample/swresample.h>
}

#include <limits>

#include "file_io.hpp"
#include "sha256.hpp"

namespace media_restore {
namespace {

constexpr uint64_t kMaxChannels = 8;
constexpr double kSilenceThresholdDbfs = -100.0;
constexpr int kTruePeakOversample = 4;
// EBU R128 / ITU-R BS.1770 gating constants.
constexpr double kAbsoluteGateLufs = -70.0;
constexpr int kLoudnessRate = 48000;
constexpr int64_t kBlockFrames = kLoudnessRate * 400 / 1000;  // 400 ms
constexpr int64_t kBlockStep = kBlockFrames / 4;              // 75% overlap
constexpr double kSilenceLufs = -100.0;

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
  // Accept only named, canonical layouts.  A bare channel count like "6" is
  // ambiguous (5.1 vs 6.0) and must not silently become a mask.
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
  std::string name(buffer);
  // FFmpeg spells 5.1 with the default channel order as "5.1"; keep the
  // canonical description verbatim so JSON output is stable.
  return name;
}

namespace {

bool fill_payload(const PcmBuffer& source, const AVChannelLayout& output_layout,
                  int output_rate, SwrContext* swr, PcmBuffer* out,
                  std::vector<Diagnostic>* diagnostics) {
  const int out_channels = output_layout.nb_channels;
  const int64_t max_frames =
      swr_get_out_samples(swr, static_cast<int>(source.frames));
  if (max_frames < 0) {
    diagnostics->push_back(make(ErrorCode::audio_decode,
                                "resampler rejected the input frame count"));
    return false;
  }
  if (max_frames > 0) {
    const __int128 samples = static_cast<__int128>(max_frames) * out_channels;
    if (samples > static_cast<__int128>(std::numeric_limits<int32_t>::max())) {
      diagnostics->push_back(make(ErrorCode::audio_too_large,
                                  "resampled audio exceeds the buffer limit"));
      return false;
    }
  }
  out->sample_rate = output_rate;
  copy_layout(output_layout, &out->layout);
  out->channels = out_channels;
  out->interleaved = false;
  out->frames = std::max<int64_t>(max_frames, 0);
  out->data.assign(static_cast<size_t>(out->frames) * static_cast<size_t>(out_channels),
                   0.0f);

  uint8_t* planes[8] = {};
  for (int c = 0; c < out_channels && c < 8; ++c) {
    planes[c] = reinterpret_cast<uint8_t*>(out->plane(c));
  }
  // Planar source addressing: swr expects one pointer per input channel.
  const uint8_t* input_planes[8] = {};
  if (source.interleaved) {
    input_planes[0] = reinterpret_cast<const uint8_t*>(source.data.data());
  } else {
    for (int c = 0; c < source.channels && c < 8; ++c) {
      input_planes[c] = reinterpret_cast<const uint8_t*>(source.plane(c));
    }
  }
  const int converted = swr_convert(
      swr, planes, static_cast<int>(out->frames), input_planes,
      static_cast<int>(source.frames));
  if (converted < 0) {
    diagnostics->push_back(make(ErrorCode::audio_decode,
                                "resampler failed: " + av_error(converted)));
    return false;
  }
  out->frames = converted;
  out->data.resize(static_cast<size_t>(converted) * static_cast<size_t>(out_channels));
  return true;
}

}  // namespace

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

  const AVSampleFormat input_format =
      input.interleaved ? AV_SAMPLE_FMT_FLT : AV_SAMPLE_FMT_FLTP;

  SwrContext* swr = nullptr;
  int error = swr_alloc_set_opts2(
      &swr, &config.output_layout, AV_SAMPLE_FMT_FLTP, config.output_rate,
      &input.layout, input_format, input.sample_rate, 0, nullptr);
  if (error < 0 || swr == nullptr) {
    diagnostics->push_back(make(ErrorCode::audio_decode,
                                "cannot allocate resampler: " + av_error(error)));
    if (swr != nullptr) swr_free(&swr);
    return false;
  }
  if (swr_init(swr) < 0) {
    diagnostics->push_back(make(ErrorCode::audio_decode, "cannot initialise resampler"));
    swr_free(&swr);
    return false;
  }

  const bool ok = fill_payload(input, config.output_layout, config.output_rate,
                               swr, out, diagnostics);
  if (ok) {
    // Flush the resampler's internal delay so no tail samples are lost.
    PcmBuffer tail;
    tail.sample_rate = config.output_rate;
    copy_layout(config.output_layout, &tail.layout);
    tail.channels = config.output_layout.nb_channels;
    tail.interleaved = false;
    const int64_t capacity = static_cast<int64_t>(config.output_rate);
    tail.frames = capacity;
    tail.data.assign(static_cast<size_t>(capacity) * tail.channels, 0.0f);
    uint8_t* planes[8] = {};
    for (int c = 0; c < tail.channels && c < 8; ++c) {
      planes[c] = reinterpret_cast<uint8_t*>(tail.plane(c));
    }
    const int flushed = swr_convert(swr, planes, static_cast<int>(capacity),
                                    nullptr, 0);
    if (flushed > 0) {
      tail.frames = flushed;
      tail.data.resize(static_cast<size_t>(flushed) * tail.channels);
      const int64_t old = out->frames;
      out->frames += flushed;
      if (static_cast<__int128>(out->frames) * out->channels >
          config.max_output_frames * kMaxChannels) {
        diagnostics->push_back(make(ErrorCode::audio_too_large,
                                    "resampled audio exceeds the sample limit"));
        swr_free(&swr);
        return false;
      }
      out->data.resize(static_cast<size_t>(out->frames) * out->channels);
      for (int c = 0; c < out->channels; ++c) {
        std::memcpy(out->plane(c) + old, tail.plane(c),
                    static_cast<size_t>(flushed) * sizeof(float));
      }
    }
    stats->resampler_delay = swr_get_delay(swr, config.output_rate);
  }
  swr_free(&swr);
  if (ok) stats->output_frames = out->frames;
  return ok;
}

namespace {

// BS.1770 channel weights: L/R/C = 1.0, Ls/Rs = 1.41, LFE = 0.
double channel_weight(const AVChannelLayout& layout, int channel) {
  if (layout.order != AV_CHANNEL_ORDER_NATIVE) return 1.0;
  const uint64_t kLfe = 1ULL << AV_CHAN_LOW_FREQUENCY;
  const uint64_t kRear = (1ULL << AV_CHAN_BACK_LEFT) | (1ULL << AV_CHAN_BACK_RIGHT) |
                         (1ULL << AV_CHAN_SIDE_LEFT) | (1ULL << AV_CHAN_SIDE_RIGHT);
  const AVChannel order[8] = {AV_CHAN_FRONT_LEFT,  AV_CHAN_FRONT_RIGHT,
                              AV_CHAN_FRONT_CENTER, AV_CHAN_LOW_FREQUENCY,
                              AV_CHAN_BACK_LEFT,  AV_CHAN_BACK_RIGHT,
                              AV_CHAN_SIDE_LEFT,  AV_CHAN_SIDE_RIGHT};
  if (channel < 0 || channel >= 8) return 1.0;
  const uint64_t bit = 1ULL << order[channel];
  if (bit & kLfe) return 0.0;
  if (bit & kRear) return 1.41;
  (void)layout;
  return 1.0;
}

// K-weighting biquads at 48 kHz (ITU-R BS.1770-4, tables 1 and 2).
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

constexpr Biquad kShelf{1.53512485958697, -2.69169618940638, 1.19839281085285,
                        -1.69065929318241, 0.73248077421585};
constexpr Biquad kHighPass{1.0, -2.0, 1.0, -1.99004745483398, 0.99007225036621};
constexpr int kMeasureRate = 48000;

// Resamples to 48 kHz planar float for measurement.  The K-weighting
// coefficients are only valid at 48 kHz, so measurement must not depend on
// the delivery rate.
bool resample_for_measurement(const PcmBuffer& input, std::vector<float>* out,
                              int64_t* frames) {
  if (input.sample_rate == kMeasureRate) {
    const size_t total = static_cast<size_t>(input.frames) * input.channels;
    out->assign(total, 0.0f);
    for (int c = 0; c < input.channels; ++c) {
      std::memcpy(out->data() + static_cast<size_t>(c) * input.frames,
                  input.plane(c), static_cast<size_t>(input.frames) * sizeof(float));
    }
    *frames = input.frames;
    return true;
  }
  SwrContext* swr = nullptr;
  AVChannelLayout layout{};
  if (av_channel_layout_copy(&layout, &input.layout) < 0) return false;
  const int error = swr_alloc_set_opts2(
      &swr, &layout, AV_SAMPLE_FMT_FLTP, kMeasureRate, &layout, AV_SAMPLE_FMT_FLTP,
      input.sample_rate, 0, nullptr);
  av_channel_layout_uninit(&layout);
  if (error < 0 || swr == nullptr) {
    if (swr) swr_free(&swr);
    return false;
  }
  if (swr_init(swr) < 0) {
    swr_free(&swr);
    return false;
  }
  const int max_frames = swr_get_out_samples(swr, static_cast<int>(input.frames));
  if (max_frames < 0) {
    swr_free(&swr);
    return false;
  }
  std::vector<float> buffer(static_cast<size_t>(max_frames) * input.channels, 0.0f);
  std::vector<uint8_t*> planes(static_cast<size_t>(input.channels));
  for (int c = 0; c < input.channels; ++c) {
    planes[static_cast<size_t>(c)] =
        reinterpret_cast<uint8_t*>(buffer.data() + static_cast<size_t>(c) * max_frames);
  }
  std::vector<const uint8_t*> in_planes(static_cast<size_t>(input.channels));
  for (int c = 0; c < input.channels; ++c) {
    in_planes[static_cast<size_t>(c)] = reinterpret_cast<const uint8_t*>(input.plane(c));
  }
  int converted = swr_convert(swr, planes.data(), max_frames, in_planes.data(),
                              static_cast<int>(input.frames));
  int flushed = 0;
  if (converted >= 0) {
    std::vector<uint8_t*> tail(static_cast<size_t>(input.channels));
    for (int c = 0; c < input.channels; ++c) {
      tail[static_cast<size_t>(c)] = reinterpret_cast<uint8_t*>(
          buffer.data() + static_cast<size_t>(c) * max_frames + converted);
    }
    flushed = swr_convert(swr, tail.data(), max_frames - converted, nullptr, 0);
  }
  swr_free(&swr);
  if (converted < 0 || flushed < 0) return false;
  // Interleave the first `converted + flushed` frames for per-channel loops.
  const int64_t produced = converted + flushed;
  std::vector<float> interleaved(static_cast<size_t>(produced) * input.channels);
  for (int c = 0; c < input.channels; ++c) {
    const float* plane = buffer.data() + static_cast<size_t>(c) * max_frames;
    for (int64_t i = 0; i < produced; ++i) {
      interleaved[static_cast<size_t>(i) * input.channels + c] = plane[i];
    }
  }
  *out = std::move(interleaved);
  *frames = produced;
  return true;
}

// True peak: 4x linear interpolation of the sample peak.  Deterministic and
// adequate for archive limiting; not a substitute for a broadcast meter.
double true_peak_dbfs(const PcmBuffer& buffer) {
  if (buffer.frames <= 0 || buffer.channels <= 0) {
    return -std::numeric_limits<double>::infinity();
  }
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
  if (peak <= 0.0) return -std::numeric_limits<double>::infinity();
  return 20.0 * std::log10(peak);
}

double sample_peak_dbfs(const PcmBuffer& buffer) {
  double peak = 0.0;
  for (int c = 0; c < buffer.channels; ++c) {
    const float* samples = buffer.plane(c);
    for (int64_t i = 0; i < buffer.frames; ++i) {
      peak = std::max(peak, std::fabs(static_cast<double>(samples[i])));
    }
  }
  if (peak <= 0.0) return -std::numeric_limits<double>::infinity();
  return 20.0 * std::log10(peak);
}

}  // namespace

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
  if (sample_peak_dbfs(buffer) < kSilenceThresholdDbfs) {
    return true;  // explicit silence path
  }
  *silent = false;

  std::vector<float> interleaved;
  int64_t frames = 0;
  if (!resample_for_measurement(buffer, &interleaved, &frames)) {
    diagnostics->push_back(make(ErrorCode::loudness_invalid,
                                "cannot prepare the buffer for loudness measurement"));
    return false;
  }
  if (frames < kBlockStep) {
    diagnostics->push_back(make(
        ErrorCode::loudness_invalid,
        "segment is shorter than one 100 ms gating block; loudness stays at the "
        "silence floor",
        {}));
    return true;
  }

  const int channels = buffer.channels;
  // Filter each channel with continuous state, then form gating blocks.
  std::vector<std::vector<double>> filtered(
      static_cast<size_t>(channels), std::vector<double>(static_cast<size_t>(frames)));
  for (int c = 0; c < channels; ++c) {
    Biquad s1 = kShelf;
    Biquad s2 = kHighPass;
    for (int64_t i = 0; i < frames; ++i) {
      const double sample =
          static_cast<double>(interleaved[static_cast<size_t>(i) * channels + c]);
      filtered[static_cast<size_t>(c)][static_cast<size_t>(i)] =
          s2.process(s1.process(sample));
    }
  }
  std::vector<double> block_levels;
  for (int64_t start = 0; start + kBlockFrames <= frames; start += kBlockStep) {
    double sum = 0.0;
    for (int c = 0; c < channels; ++c) {
      double energy = 0.0;
      const auto& channel = filtered[static_cast<size_t>(c)];
      for (int64_t i = start; i < start + kBlockFrames; ++i) {
        energy += channel[static_cast<size_t>(i)] * channel[static_cast<size_t>(i)];
      }
      energy /= static_cast<double>(kBlockFrames);
      sum += channel_weight(buffer.layout, c) * energy;
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
  if (!config.apply_gain || stats->short_segment) {
    stats->output_lufs = lufs;
    stats->output_true_peak_dbtp = true_peak;
    return true;
  }

  // Gain to reach the target.  Clamp to a sane range so a near-silent but
  // non-zero segment cannot be amplified without bound.
  double gain_db = config.target_lufs - lufs;
  constexpr double kMaxGainDb = 30.0;
  constexpr double kMinGainDb = -30.0;
  gain_db = std::clamp(gain_db, kMinGainDb, kMaxGainDb);
  const double gain = std::pow(10.0, gain_db / 20.0);
  for (int c = 0; c < buffer->channels; ++c) {
    float* samples = buffer->plane(c);
    for (int64_t i = 0; i < buffer->frames; ++i) {
      samples[i] = static_cast<float>(samples[i] * gain);
    }
  }
  stats->applied_gain_db = gain_db;

  // True-peak limiting at the configured ceiling.
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

bool apply_timeline_offset(int64_t offset_samples, PcmBuffer* buffer,
                           AudioStats* stats) {
  if (offset_samples == 0) return true;
  if (offset_samples > 0) {
    const int64_t frames = offset_samples;
    if (static_cast<__int128>(buffer->frames + frames) * buffer->channels >
        std::numeric_limits<int64_t>::max()) {
      return false;
    }
    const int64_t old = buffer->frames;
    PcmBuffer shifted;
    shifted.sample_rate = buffer->sample_rate;
    copy_layout(buffer->layout, &shifted.layout);
    shifted.channels = buffer->channels;
    shifted.interleaved = false;
    shifted.frames = old + frames;
    shifted.data.assign(static_cast<size_t>(shifted.frames) * shifted.channels, 0.0f);
    for (int c = 0; c < buffer->channels; ++c) {
      std::memcpy(shifted.plane(c) + frames, buffer->plane(c),
                  static_cast<size_t>(old) * sizeof(float));
    }
    *buffer = std::move(shifted);
    stats->padded_frames = frames;
    return true;
  }
  // Negative offset: the source starts before the segment; drop the leading
  // samples so the first output sample aligns with the segment start.
  const int64_t drop = -offset_samples;
  if (drop >= buffer->frames) {
    for (int c = 0; c < buffer->channels; ++c) {
      std::fill(buffer->plane(c), buffer->plane(c) + buffer->frames, 0.0f);
    }
    stats->discarded_frames = buffer->frames;
    buffer->frames = 0;
    buffer->data.assign(static_cast<size_t>(buffer->channels) * 0, 0.0f);
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
  std::string& wav = *out;
  wav.clear();
  wav.reserve(static_cast<size_t>(data_bytes) + 44);
  wav.append("RIFF", 4);
  put_u32(&wav, static_cast<uint32_t>(36 + data_bytes));
  wav.append("WAVE", 4);
  wav.append("fmt ", 4);
  put_u32(&wav, 16);
  put_u16(&wav, 3);  // IEEE float
  put_u16(&wav, static_cast<uint16_t>(buffer.channels));
  put_u32(&wav, static_cast<uint32_t>(buffer.sample_rate));
  const uint32_t block_align = static_cast<uint32_t>(buffer.channels) * 4;
  put_u32(&wav, static_cast<uint32_t>(buffer.sample_rate) * block_align);
  put_u16(&wav, static_cast<uint16_t>(block_align));
  put_u16(&wav, 32);
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
  if (decoder == nullptr) {
    avformat_close_input(&format);
    diagnostics->push_back(make(ErrorCode::audio_decode,
                                "no decoder for the audio codec",
                                {.file = path.string()}));
    return false;
  }
  AVCodecContext* codec = avcodec_alloc_context3(decoder);
  if (codec == nullptr) {
    avformat_close_input(&format);
    diagnostics->push_back(make(ErrorCode::audio_decode, "cannot allocate decoder"));
    return false;
  }
  if (avcodec_parameters_to_context(codec, stream->codecpar) < 0 ||
      avcodec_open2(codec, decoder, nullptr) < 0) {
    avcodec_free_context(&codec);
    avformat_close_input(&format);
    diagnostics->push_back(make(ErrorCode::audio_decode, "cannot open decoder",
                                {.file = path.string()}));
    return false;
  }

  PcmBuffer result;
  result.sample_rate = codec->sample_rate;
  copy_layout(codec->ch_layout, &result.layout);
  result.channels = result.layout.nb_channels;
  result.interleaved = false;
  if (result.channels <= 0) {
    avcodec_free_context(&codec);
    avformat_close_input(&format);
    diagnostics->push_back(make(ErrorCode::audio_unsupported_layout,
                                "decoded audio has no channel layout",
                                {.file = path.string()}));
    return false;
  }

  // Convert every decoded frame into planar float at the source rate/layout
  // so the rest of the pipeline has one sample representation.
  SwrContext* swr = nullptr;
  AVChannelLayout layout{};
  if (av_channel_layout_copy(&layout, &result.layout) < 0) {
    avcodec_free_context(&codec);
    avformat_close_input(&format);
    diagnostics->push_back(make(ErrorCode::audio_decode, "cannot copy layout"));
    return false;
  }
  const int swr_error = swr_alloc_set_opts2(
      &swr, &layout, AV_SAMPLE_FMT_FLTP, result.sample_rate, &layout,
      static_cast<AVSampleFormat>(codec->sample_fmt), result.sample_rate, 0, nullptr);
  av_channel_layout_uninit(&layout);
  AVPacket* packet = av_packet_alloc();
  AVFrame* frame = av_frame_alloc();
  bool ok = swr != nullptr && swr_error >= 0 && swr_init(swr) >= 0 &&
            packet != nullptr && frame != nullptr;
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
      const int capacity = swr_get_out_samples(swr, frame->nb_samples);
      if (capacity <= 0) continue;
      if (static_cast<__int128>(result.frames + capacity) * result.channels >
          static_cast<__int128>(limits.max_audio_samples) * kMaxChannels) {
        ok = false;
        diagnostics->push_back(make(ErrorCode::audio_too_large,
                                    "decoded audio exceeds the sample limit",
                                    {.file = path.string()}));
        break;
      }
      const int64_t old = result.frames;
      result.frames += capacity;
      result.data.resize(static_cast<size_t>(result.frames) * result.channels, 0.0f);
      std::vector<uint8_t*> planes(static_cast<size_t>(result.channels));
      for (int c = 0; c < result.channels; ++c) {
        planes[static_cast<size_t>(c)] = reinterpret_cast<uint8_t*>(
            result.plane(c) + old);
      }
      const int converted = swr_convert(swr, planes.data(), capacity,
                                        const_cast<const uint8_t**>(frame->extended_data),
                                        frame->nb_samples);
      if (converted < 0) {
        ok = false;
        diagnostics->push_back(make(ErrorCode::audio_decode,
                                    "cannot convert decoded samples: " +
                                        av_error(converted),
                                    {.file = path.string()}));
        break;
      }
      result.frames = old + converted;
      result.data.resize(static_cast<size_t>(result.frames) * result.channels);
      av_frame_unref(frame);
    }
  }
  if (ok) {
    // Flush decoder and resampler tails.
    avcodec_send_packet(codec, nullptr);
    while (true) {
      const int received = avcodec_receive_frame(codec, frame);
      if (received < 0) break;
      const int capacity = swr_get_out_samples(swr, frame->nb_samples);
      if (capacity > 0) {
        const int64_t old = result.frames;
        if (static_cast<__int128>(old + capacity) * result.channels >
            static_cast<__int128>(limits.max_audio_samples) * kMaxChannels) {
          break;
        }
        result.frames += capacity;
        result.data.resize(static_cast<size_t>(result.frames) * result.channels, 0.0f);
        std::vector<uint8_t*> planes(static_cast<size_t>(result.channels));
        for (int c = 0; c < result.channels; ++c) {
          planes[static_cast<size_t>(c)] =
              reinterpret_cast<uint8_t*>(result.plane(c) + old);
        }
        const int converted = swr_convert(
            swr, planes.data(), capacity,
            const_cast<const uint8_t**>(frame->extended_data), frame->nb_samples);
        result.frames = old + (converted > 0 ? converted : 0);
        result.data.resize(static_cast<size_t>(result.frames) * result.channels);
      }
      av_frame_unref(frame);
    }
  }
  if (swr != nullptr) swr_free(&swr);
  av_packet_free(&packet);
  av_frame_free(&frame);
  avcodec_free_context(&codec);
  avformat_close_input(&format);
  if (!ok) return false;
  result.data.resize(static_cast<size_t>(result.frames) * result.channels);
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
    int64_t offset_samples = 0;
    if (!Rational::sample_count(Rational(offset_ticks * time_base.num,
                                         time_base.den),
                                config.output_rate, &offset_samples)) {
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

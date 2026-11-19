#pragma once

#include <cstdint>
#include <cstddef>
#include <filesystem>
#include <string>
#include <string_view>
#include <vector>

extern "C" {
#include <libavutil/channel_layout.h>
}

#include "error.hpp"
#include "manifest.hpp"

namespace media_restore {

// A bounded PCM buffer (interleaved or planar) produced by the restore step.
// Samples are stored as float in [-1, 1] which keeps loudness maths and
// resampler output in one representation.
struct PcmBuffer {
  int sample_rate = 0;
  AVChannelLayout layout{};
  bool interleaved = false;
  int channels = 0;
  int64_t frames = 0;
  std::vector<float> data;

  PcmBuffer();
  PcmBuffer(const PcmBuffer& other);
  PcmBuffer& operator=(const PcmBuffer& other);
  PcmBuffer(PcmBuffer&& other) noexcept;
  PcmBuffer& operator=(PcmBuffer&& other) noexcept;
  ~PcmBuffer();

  // Planar: plane(c) points at `frames` floats for channel c.
  // Interleaved: plane(0) points at the whole buffer and frame(i) is the
  // first sample of frame i.
  float* plane(int channel);
  float* frame(int64_t index);
  const float* plane(int channel) const;
  const float* frame(int64_t index) const;
};

// Parsing of the CLI layout names.  Only layouts with a canonical FFmpeg
// channel layout are accepted; anything else is E_AUDIO_UNSUPPORTED_LAYOUT.
bool parse_layout(std::string_view name, AVChannelLayout* out);
// Canonical name for a layout, e.g. "5.1", used for stable output.
std::string layout_name(const AVChannelLayout& layout);

struct AudioRestoreConfig {
  int output_rate = 48000;
  AVChannelLayout output_layout{};
  double target_lufs = -23.0;
  double true_peak_dbtp = -1.0;
  bool apply_gain = true;
  int64_t max_output_frames = 48000LL * 60 * 30;

  AudioRestoreConfig();
  AudioRestoreConfig(const AudioRestoreConfig&) = delete;
  AudioRestoreConfig& operator=(const AudioRestoreConfig&) = delete;
  AudioRestoreConfig(AudioRestoreConfig&&) noexcept;
  AudioRestoreConfig& operator=(AudioRestoreConfig&&) noexcept;
  ~AudioRestoreConfig();
};

struct AudioStats {
  int input_rate = 0;
  int output_rate = 0;
  std::string input_layout;
  std::string output_layout;
  int64_t input_frames = 0;
  int64_t output_frames = 0;
  int64_t resampler_delay = 0;
  int64_t discarded_frames = 0;   // tail samples the destination could not hold
  int64_t padded_frames = 0;      // silence prepended for a positive offset
  int64_t silence_frames = 0;     // silent frames detected at the input
  double input_lufs = 0.0;
  double output_lufs = 0.0;
  double input_true_peak_dbtp = 0.0;
  double output_true_peak_dbtp = 0.0;
  int64_t limited_samples = 0;
  double applied_gain_db = 0.0;
  // Exact media-time error introduced when the timeline offset was rounded to
  // the output sample grid (display only; the rounding itself is integer).
  double offset_rounding_seconds = 0.0;
  bool silent = false;
  bool short_segment = false;
  bool loudness_measured = false;
  bool peak_limited = false;
};

// Converts `input` to the configured rate/layout.  Handles planar and
// interleaved input, any channel count up to 7.1, and a non-frame-aligned
// tail.  Counts discarded/padded samples in `stats`.
bool convert_audio(const PcmBuffer& input, const AudioRestoreConfig& config,
                   PcmBuffer* out, AudioStats* stats,
                   std::vector<Diagnostic>* diagnostics);

// Applies a timeline offset in samples.  Positive offsets prepend silence
// (audio starts later than the segment); negative offsets trim leading
// samples.  Counts padded/discarded frames.
bool apply_timeline_offset(int64_t offset_samples, PcmBuffer* buffer,
                           AudioStats* stats);

// Applies loudness normalisation to `target_lufs` followed by a true-peak
// limiter at `true_peak_dbtp`.  Silence, too-short and out-of-range material
// take explicit paths instead of producing NaN or inf.
bool normalize_loudness(const AudioRestoreConfig& config, PcmBuffer* buffer,
                        AudioStats* stats,
                        std::vector<Diagnostic>* diagnostics);

// Integrated loudness (LUFS) and true peak (dBTP) of a buffer, measured with
// FFmpeg's EBU R128 filter so the numbers match ffmpeg -af loudnorm.
bool measure_loudness(const PcmBuffer& buffer, double* lufs, double* true_peak,
                      bool* silent, std::vector<Diagnostic>* diagnostics);

// Full per-segment pipeline: decode, convert, shift by the timeline offset,
// normalise, then serialize the result as a deterministic WAV payload.
struct RestoredAudio {
  std::string wav;          // complete RIFF/WAVE file
  AudioStats stats;
};

bool restore_segment_audio(const std::filesystem::path& source,
                           const Rational& offset, const Rational& time_base,
                           const AudioRestoreConfig& config,
                           RestoredAudio* out,
                           std::vector<Diagnostic>* diagnostics);

// Decodes an audio file through libavformat/libavcodec into float planar at
// its native rate/layout.  WAV fixtures exercise 16/24/32-bit PCM and float.
bool decode_audio_file(const std::filesystem::path& path, const Limits& limits,
                       PcmBuffer* out, std::vector<Diagnostic>* diagnostics);

// Canonical float32 WAV encoding with a stable RIFF header.
bool encode_wav(const PcmBuffer& buffer, std::string* out,
                std::vector<Diagnostic>* diagnostics);

}  // namespace media_restore

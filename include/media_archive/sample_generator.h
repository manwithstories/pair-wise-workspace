#pragma once
// Sample generation: real libavcodec-encoded H.264/H.265/AV1 elementary
// streams fed through the deterministic transport trace, plus a matching
// packet manifest and a fragmented PCM audio archive. Everything is derived
// from a seed; no external or pre-existing media is referenced.

#include <cstdint>
#include <string>
#include <vector>

#include "media_archive/audio_sync.h"
#include "media_archive/error.h"
#include "media_archive/transport_trace.h"
#include "media_archive/types.h"

namespace media_archive {

struct SampleConfig {
  uint64_t seed = 42;
  CodecType codec = CodecType::H264;
  uint32_t frame_count = 240;  // ~8 s at 30 fps
  uint32_t width = 176;
  uint32_t height = 144;
  uint32_t gop = 30;           // one keyframe per second
  uint32_t fps = 30;
  uint32_t audio_rate = 24000;
  TraceConfig trace;
};

struct EncodedFrame {
  std::vector<uint8_t> bytes;
  bool keyframe = false;
};

struct GeneratedSample {
  CodecType codec = CodecType::H264;
  std::vector<EncodedFrame> frames;   // full encoded output, display order
  TransportTrace trace;               // trace with real frame sizes
  AudioArchive audio;
  std::vector<uint8_t> received_video;// present frames concatenated
  uint64_t timeline_end_pts = 0;
};

// Encode video, synthesize audio and run the transport simulation.
Diagnostic generate_sample(const SampleConfig& config,
                           GeneratedSample* sample);

// Write stream.<codec>, packets.csv and audio.pcm into `dir`.
Diagnostic write_sample_bundle(const std::string& dir,
                               const GeneratedSample& sample);

// File extension used for a codec's raw elementary stream.
const char* codec_extension(CodecType codec);

}  // namespace media_archive

#include "media_archive/sample_generator.h"

namespace media_archive {

const char* codec_extension(CodecType codec) {
  switch (codec) {
    case CodecType::H264: return "h264";
    case CodecType::H265: return "h265";
    case CodecType::AV1:  return "av1";
    case CodecType::Unknown: return "bin";
  }
  return "bin";
}

Diagnostic generate_sample(const SampleConfig& config,
                           GeneratedSample* sample) {
  // Implemented in the following generator batches.
  (void)config;
  (void)sample;
  return diag(Status::UsageError, "sample generator not implemented yet");
}

Diagnostic write_sample_bundle(const std::string& dir,
                               const GeneratedSample& sample) {
  (void)dir;
  (void)sample;
  return diag(Status::UsageError, "sample bundle writer not implemented yet");
}

}  // namespace media_archive

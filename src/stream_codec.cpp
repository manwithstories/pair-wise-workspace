#include "media/stream_reader.hpp"

extern "C" {
#include <libavformat/avformat.h>
}

namespace media {

CodecType probe_codec(const std::vector<std::uint8_t>& bytes) noexcept {
  if (bytes.empty()) return CodecType::Unknown;

  // Local structural heuristic first: streams begin with an Annex B start code
  // followed by an identifiable H.264/H.265 NAL, or with an AV1 temporal unit
  // (4-byte big-endian length then an OBU header carrying obu_type 1/3).
  if (bytes.size() >= 5 && bytes[0] == 0 && bytes[1] == 0 &&
      (bytes[2] == 1 || (bytes[2] == 0 && bytes[3] == 1))) {
    const std::size_t hdr = (bytes[2] == 1) ? 3 : 4;
    const std::uint8_t b0 = bytes[hdr];
    const std::uint8_t h264_type = b0 & 0x1f;
    const std::uint8_t h265_type = (b0 >> 1) & 0x3f;
    if (h264_type == 7 || h264_type == 5 || h264_type == 1)
      return CodecType::H264;
    if (h265_type == 33 || h265_type == 19 || h265_type == 1)
      return CodecType::H265;
  }
  if (bytes.size() >= 6) {
    const std::uint32_t len = (std::uint32_t(bytes[0]) << 24) |
                              (std::uint32_t(bytes[1]) << 16) |
                              (std::uint32_t(bytes[2]) << 8) | bytes[3];
    if (len >= 2 && len < bytes.size()) {
      const std::uint8_t obu_type = (bytes[4] >> 3) & 0x0f;
      if (obu_type == 1 || obu_type == 3) return CodecType::AV1;
    }
  }

  // Fall back to libavformat's general-purpose format probe.
  AVProbeData pd{};
  const char* filename = "sample";
  pd.filename = filename;
  pd.buf_size = static_cast<int>(
      bytes.size() < 4096 ? bytes.size() + 1 : 4096);
  std::vector<std::uint8_t> buf(pd.buf_size, 0);
  const std::size_t copy = pd.buf_size - 1;
  for (std::size_t i = 0; i < copy; ++i) buf[i] = bytes[i];
  pd.buf = reinterpret_cast<unsigned char*>(buf.data());

  const AVInputFormat* fmt = av_probe_input_format(&pd, 1);
  if (!fmt || !fmt->name) return CodecType::Unknown;
  const std::string name = fmt->name;
  if (name.find("h264") != std::string::npos ||
      name.find("avc") != std::string::npos) return CodecType::H264;
  if (name.find("hevc") != std::string::npos ||
      name.find("h265") != std::string::npos) return CodecType::H265;
  if (name.find("av1") != std::string::npos) return CodecType::AV1;
  return CodecType::Unknown;
}

} // namespace media

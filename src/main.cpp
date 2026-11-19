#include <cstdio>
#include <cstring>
#include <string>
#include <vector>

#include "media_archive/error.h"
#include "media_archive/types.h"

namespace media_archive {
const char* codec_name(CodecType codec) {
  switch (codec) {
    case CodecType::H264: return "h264";
    case CodecType::H265: return "h265";
    case CodecType::AV1:  return "av1";
    case CodecType::Unknown: return "unknown";
  }
  return "unknown";
}

CodecType codec_from_name(const std::string& name) {
  if (name == "h264" || name == "avc") return CodecType::H264;
  if (name == "h265" || name == "hevc") return CodecType::H265;
  if (name == "av1") return CodecType::AV1;
  return CodecType::Unknown;
}

double ticks_to_ms(uint64_t ticks) {
  return static_cast<double>(ticks) * 1000.0 /
         static_cast<double>(kTimebaseHz);
}

uint32_t crc32_ieee(const uint8_t* data, size_t len) {
  static uint32_t table[256];
  static bool initialized = false;
  if (!initialized) {
    for (uint32_t i = 0; i < 256; ++i) {
      uint32_t c = i;
      for (int k = 0; k < 8; ++k)
        c = (c & 1u) ? (0xEDB88320u ^ (c >> 1)) : (c >> 1);
      table[i] = c;
    }
    initialized = true;
  }
  uint32_t crc = 0xFFFFFFFFu;
  for (size_t i = 0; i < len; ++i)
    crc = table[(crc ^ data[i]) & 0xFFu] ^ (crc >> 8);
  return crc ^ 0xFFFFFFFFu;
}
}  // namespace media_archive

namespace {
void print_help() {
  std::printf(
      "media-archive — long-term media archive recovery tool\n\n"
      "Usage:\n"
      "  media-archive generate --seed N --out DIR [--codec h264|h265|av1]\n"
      "  media-archive recover --codec C --input F --packets F --audio F\n"
      "                        --index F --checkpoint D [--report text|json]\n"
      "                        [--crash-after N] [--max-input-bytes N]\n"
      "  media-archive verify --index F --checkpoint D\n\n"
      "Exit codes: 0 success, 2 usage/argument error,\n"
      "            3 media corruption, 4 checkpoint/write failure\n");
}
}  // namespace

int main(int argc, char** argv) {
  std::vector<std::string> args(argv + 1, argv + argc);
  if (args.empty() || args[0] == "-h" || args[0] == "--help") {
    print_help();
    return args.empty() ? static_cast<int>(media_archive::ExitCode::UsageError)
                        : 0;
  }
  const std::string command = args.front();
  if (command == "generate" || command == "recover" || command == "verify") {
    // Implemented in later module batches.
    std::fprintf(stderr, "command '%s' not implemented yet\n", command.c_str());
    return static_cast<int>(media_archive::ExitCode::UsageError);
  }
  std::fprintf(stderr, "unknown command: %s\n", command.c_str());
  return static_cast<int>(media_archive::ExitCode::UsageError);
}

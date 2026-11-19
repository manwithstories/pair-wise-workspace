#include <cstdio>
#include <string>
#include <vector>

#include "media_archive/error.h"
#include "media_archive/types.h"

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

#include <cinttypes>
#include <cstdio>
#include <exception>
#include <string>
#include <vector>

#include "cli_options.hpp"
#include "commands.hpp"
#include "media_types.hpp"

namespace {

using namespace mfs;

constexpr const char* kUsage =
    "usage:\n"
    "  media_repair generate --seed N --frames N --loss-burst N\n"
    "                        --duplicate-rate R --reorder-window N --out FILE\n"
    "                        [--width N] [--height N] [--keyframe-interval N]\n"
    "                        [--loss-events N] [--timestamp-defects N]\n"
    "                        [--arrival-gap N ...]\n"
    "  media_repair repair   --input FILE --segment-frames N --json FILE\n"
    "                        [--max-segments N] [--max-frame-bytes N]\n"
    "                        [--max-total-bytes N] [--reorder-window N]\n"
    "  media_repair verify   --input FILE --report FILE\n"
    "  media_repair inspect  --input FILE\n"
    "  media_repair damage   --input FILE --kind KIND --out FILE\n"
    "                        (checksum|length|header|tail|empty|magic|required)\n";

}  // namespace

int main(int argc, char** argv) {
  if (argc < 2) {
    std::fprintf(stderr, "%s", kUsage);
    return kExitUsageOrFormat;
  }
  const std::string command = argv[1];
  std::vector<std::string> tokens;
  tokens.reserve(static_cast<size_t>(argc));
  for (int i = 2; i < argc; ++i) {
    tokens.emplace_back(argv[i]);
  }
  OptionTable options;
  std::string error;
  if (!parse_options(tokens, options, error)) {
    std::fprintf(stderr, "%s\n%s", error.c_str(), kUsage);
    return kExitUsageOrFormat;
  }
  if (command == "--help" || command == "-h" || command == "help") {
    std::printf("%s", kUsage);
    return kExitOk;
  }
  try {
    if (command == "generate") {
      return cmd_generate(options);
    }
    if (command == "repair") {
      return cmd_repair(options);
    }
    if (command == "verify") {
      return cmd_verify(options);
    }
    if (command == "inspect") {
      return cmd_inspect(options);
    }
    if (command == "damage") {
      return cmd_damage(options);
    }
  } catch (const mfs::MediaError& error) {
    std::fprintf(stderr, "error: %s (offset %" PRIu64 ")\n", error.what(),
                 error.offset());
    return kExitUsageOrFormat;
  } catch (const std::exception& error) {
    std::fprintf(stderr, "internal error: %s\n", error.what());
    return kExitInternal;
  }
  std::fprintf(stderr, "unknown command: %s\n%s", command.c_str(), kUsage);
  return kExitUsageOrFormat;
}

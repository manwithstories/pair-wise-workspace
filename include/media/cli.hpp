#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "media/index.hpp"
#include "media/json.hpp"
#include "media/manifest.hpp"
#include "media/pipeline.hpp"

namespace mediaqc {

// Exit codes promised by the command-line contract.
inline constexpr int kExitOk = 0;
inline constexpr int kExitUsage = 2;     // invalid arguments or input
inline constexpr int kExitMedia = 3;     // material damage found
inline constexpr int kExitIo = 4;        // I/O or recovery failure

// `mediaqc generate` writes a sample delivery.
int run_generate(const std::vector<std::string>& args);

// `mediaqc process` repairs a delivery into an output directory and prints the
// summary line the acceptance checks parse.
int run_process(const std::vector<std::string>& args);

// `mediaqc verify` independently re-checks an output directory.
int run_verify(const std::vector<std::string>& args);

// `mediaqc query` answers time/frame lookups against an index.
int run_query(const std::vector<std::string>& args);

void print_usage();

}  // namespace mediaqc

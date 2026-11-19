// Command line interface. Implemented incrementally across module batches.
#pragma once

#include <string>
#include <vector>

namespace media {

// Returns the process exit code (0 ok, 2 corrupt input, 3 checkpoint, 64 usage).
int run_cli(const std::vector<std::string>& args);

} // namespace media

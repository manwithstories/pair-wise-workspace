#pragma once

// Command handlers for the terminal interface. Each returns one of the exit
// codes defined below; usage/format problems return 2 and internal failures 3.

#include "cli_options.hpp"

namespace mfs {

// Exit codes fixed by the acceptance contract.
inline constexpr int kExitOk = 0;
inline constexpr int kExitThreshold = 1;
inline constexpr int kExitUsageOrFormat = 2;
inline constexpr int kExitInternal = 3;

int cmd_generate(const OptionTable& options);
int cmd_repair(const OptionTable& options);
int cmd_verify(const OptionTable& options);
int cmd_damage(const OptionTable& options);
int cmd_inspect(const OptionTable& options);

}  // namespace mfs

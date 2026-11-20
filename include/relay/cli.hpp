#pragma once

#include <string>
#include <vector>

#include "relay/pipeline.hpp"

namespace relay::cli {

struct CommandLine {
  std::string command;  // "demo" | "resume" | "help"
  PipelineConfig config;
  bool show_help = false;
  std::string error;
};

/// Parses `demo` / `resume` arguments.  Unknown or malformed options set
/// `error`; the caller maps that to exit code 2.
CommandLine parse(const std::vector<std::string>& arguments);

int execute(const CommandLine& command_line);

std::string usage();

}  // namespace relay::cli

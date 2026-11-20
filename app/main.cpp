#include <exception>
#include <iostream>
#include <string>
#include <vector>

#include "relay/cli.hpp"

int main(int argc, char** argv) {
  std::vector<std::string> arguments;
  arguments.reserve(static_cast<std::size_t>(argc > 1 ? argc - 1 : 0));
  for (int i = 1; i < argc; ++i) {
    arguments.emplace_back(argv[i]);
  }
  try {
    const relay::cli::CommandLine command = relay::cli::parse(arguments);
    return relay::cli::execute(command);
  } catch (const std::exception& error) {
    std::cerr << "relay: fatal: " << error.what() << '\n';
    return static_cast<int>(relay::ExitCode::kCorrupt);
  }
}

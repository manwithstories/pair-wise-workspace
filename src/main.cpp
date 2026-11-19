// Media replay command line entry point.
// Exit codes: 0 ok, 2 input/index corrupt, 3 checkpoint unrecoverable, 64 usage.
#include <cstdio>
#include <string>
#include <vector>

#include "media/cli.hpp"

int main(int argc, char** argv) {
  std::vector<std::string> args;
  args.reserve(static_cast<size_t>(argc));
  for (int i = 1; i < argc; ++i) args.emplace_back(argv[i]);
  return media::run_cli(args);
}

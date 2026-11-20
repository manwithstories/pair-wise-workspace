#include <cstdio>
#include <string>
#include <vector>

#include "media/cli.hpp"

int main(int argc, char** argv) {
  if (argc < 2) {
    mediaqc::print_usage();
    return mediaqc::kExitUsage;
  }
  std::vector<std::string> args(argv + 2, argv + argc);
  const std::string command = argv[1];
  if (command == "generate") return mediaqc::run_generate(args);
  if (command == "process") return mediaqc::run_process(args);
  if (command == "verify") return mediaqc::run_verify(args);
  if (command == "query") return mediaqc::run_query(args);
  if (command == "help" || command == "--help" || command == "-h") {
    mediaqc::print_usage();
    return mediaqc::kExitOk;
  }
  std::fprintf(stderr, "mediaqc: unknown command '%s'\n", command.c_str());
  mediaqc::print_usage();
  return mediaqc::kExitUsage;
}

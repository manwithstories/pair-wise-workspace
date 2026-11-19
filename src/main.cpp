// mediaqc -- media QC / repair command line entry point.
//
// Subcommands are implemented in later modules; this file only wires up the
// argument parser and exit-code contract so the skeleton builds.
#include <cstdio>
#include <string>
#include <vector>

namespace {

int usage() {
  std::fputs(
      "usage: mediaqc <command> [options]\n"
      "  selftest --generate-fixtures --work DIR\n"
      "  batch --input DIR --output DIR [--resume] [--json]\n"
      "  verify --input DIR [--json]\n",
      stderr);
  return 2;
}

}  // namespace

int main(int argc, char** argv) {
  std::vector<std::string> args(argv + 1, argv + argc);
  if (args.empty()) return usage();
  std::fprintf(stderr, "mediaqc: command '%s' is not implemented yet\n", args[0].c_str());
  return 2;
}

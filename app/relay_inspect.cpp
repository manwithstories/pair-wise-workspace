#include <cstdio>
#include "relay/version.hpp"

int main() {
  std::fprintf(stderr, "relay_inspect: skeleton build, no subcommands yet\n");
  std::fprintf(stderr, "%s\n", relay::version_string());
  return 2;
}

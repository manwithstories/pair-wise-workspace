#include <cstdio>

// Skeleton CLI; full acceptance adapter lands in a follow-up commit.
int main(int argc, char** argv) {
  if (argc < 2) {
    std::fprintf(stderr, "usage: relay_check --input <frames> --output <dir>\n");
    return 2;
  }
  return 0;
}

// 夹具生成可执行程序（CTest 也调用它）。
// 用法: generate_fixture <output> <frames> <slices> <interlaced 0|1>
#include <cstdio>
#include <string>

#include "fixture_writer.h"

int main(int argc, char **argv) {
  if (argc != 5) {
    std::fprintf(stderr,
                 "usage: %s <output.264> <frames> <slices> <interlaced>\n",
                 argv[0]);
    return 2;
  }
  h264::FixtureParams p;
  p.frames = static_cast<uint32_t>(std::stoul(argv[2]));
  p.slices_per_frame = static_cast<uint32_t>(std::stoul(argv[3]));
  p.interlaced = std::string(argv[4]) != "0";
  if (p.frames == 0 || p.slices_per_frame == 0) {
    std::fprintf(stderr, "frames and slices must be positive\n");
    return 2;
  }

  auto data = h264::generateFixture(p); // 确定性生成
  if (!h264::FixtureWriter::saveBytes(argv[1], data)) {
    std::fprintf(stderr, "failed to write %s\n", argv[1]);
    return 2;
  }
  std::printf("wrote %zu bytes to %s (frames=%u slices=%u interlaced=%d)\n",
              data.size(), argv[1], p.frames, p.slices_per_frame,
              p.interlaced ? 1 : 0);
  return 0;
}

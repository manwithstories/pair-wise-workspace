#include "cli/Commands.h"
#include "Errors.h"
#include "image/FrameIO.h"
#include "lut/LutParser.h"
#include "pipeline/CpuPath.h"
#include "report/Histogram.h"
#include <algorithm>
#include <cstdio>
#include <filesystem>
#include <fstream>

namespace lg {
namespace {

void writeCube(const std::string& path, int n,
               const std::function<void(double, double, double, double*)>& fn) {
  std::ofstream out(path);
  if (!out)
    throw IoError("cannot write LUT: " + path);
  out << "TITLE \"generated\"\nLUT_3D_SIZE " << n
      << "\nDOMAIN_MIN 0 0 0\nDOMAIN_MAX 1 1 1\n";
  for (int b = 0; b < n; ++b)
    for (int g = 0; g < n; ++g)
      for (int r = 0; r < n; ++r) {
        double v[3];
        fn(r / (n - 1.0), g / (n - 1.0), b / (n - 1.0), v);
        char buf[128];
        std::snprintf(buf, sizeof(buf), "%.6f %.6f %.6f\n", v[0], v[1], v[2]);
        out << buf;
      }
  if (!out)
    throw IoError("failed writing LUT: " + path);
}

void identity(double r, double g, double b, double* v) {
  v[0] = r;
  v[1] = g;
  v[2] = b;
}

void tealOrange(double r, double g, double b, double* v) {
  double lum = 0.2126 * r + 0.7152 * g + 0.0722 * b;
  double sh = 1.0 - lum, hi = lum;
  v[0] = r + 0.10 * hi - 0.06 * sh;
  v[1] = g + 0.02 * hi + 0.03 * sh;
  v[2] = b - 0.08 * hi + 0.07 * sh;
  for (int c = 0; c < 3; ++c)
    v[c] = std::min(1.0, std::max(0.0, v[c]));
}

void printHist(const char* name, const ChannelStats& ch) {
  std::printf("%s=", name);
  for (std::size_t i = 0; i < ch.buckets.size(); ++i)
    std::printf("%s%llu", i ? " " : "",
                static_cast<unsigned long long>(ch.buckets[i]));
  std::printf("\n");
}

} // namespace

int cmdDemo(const Args& args) {
  namespace fs = std::filesystem;
  std::string dir = args.require("out-dir");
  fs::create_directories(dir);
  fs::create_directories(dir + "/frames");

  writeCube(dir + "/identity17.cube", 17, identity);
  writeCube(dir + "/teal_orange33.cube", 33, tealOrange);
  writeFrame(makeGradient(512, 512, 0.0), dir + "/gradient.ppm");
  for (int i = 0; i < 48; ++i) {
    char name[64];
    std::snprintf(name, sizeof(name), "/frames/frame_%03d.ppm", i);
    writeFrame(makeGradient(320, 180, i * 0.13), dir + name);
  }
  std::printf("generated %s/identity17.cube %s/teal_orange33.cube "
              "%s/gradient.ppm %s/frames (48 frames)\n",
              dir.c_str(), dir.c_str(), dir.c_str(), dir.c_str());

  // Self-check: identity LUT through the float path must be lossless.
  LutInfo info = parseLut(dir + "/identity17.cube");
  PipelineOptions opts;
  Kernel kernel(opts.interp, opts.edge);
  CpuPath cpu(info.table, kernel, opts);
  Frame input = readFrame(dir + "/gradient.ppm");
  Frame output = cpu.process(input);
  CompareResult cmp = compareFrames(input, output);
  FrameStats histIn = computeStats(input);
  FrameStats histOut = computeStats(output);

  std::printf("grid=%d entries=%zu max_dev_r=%.6f max_dev_g=%.6f "
              "max_dev_b=%.6f\n",
              info.table.size, info.table.entries(), cmp.maxDev[0],
              cmp.maxDev[1], cmp.maxDev[2]);
  printHist("hist_in_r", histIn.ch[0]);
  printHist("hist_out_r", histOut.ch[0]);

  for (int c = 0; c < 3; ++c)
    if (cmp.maxDev[c] > 1e-5)
      throw InternalError("identity LUT self-check failed");
  return kExitOk;
}

} // namespace lg
#include <fstream>
#include <functional>

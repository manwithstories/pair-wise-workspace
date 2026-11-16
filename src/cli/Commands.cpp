#include "cli/Commands.h"
#include "Errors.h"
#include "lut/LutParser.h"
#include "pipeline/CpuPath.h"
#include "pipeline/FixedPath.h"
#include "report/Histogram.h"
#include "report/Json.h"
#include <algorithm>
#include <atomic>
#include <cstdio>
#include <filesystem>
#include <thread>
#include <vector>

namespace lg {
namespace {

Interp parseInterp(const std::string& s) {
  if (s == "trilinear") return Interp::Trilinear;
  if (s == "tetrahedral" || s == "tet") return Interp::Tetrahedral;
  throw ArgsError("unknown --interp: " + s + " (trilinear|tetrahedral)");
}

Edge parseEdge(const std::string& s) {
  if (s == "clamp") return Edge::Clamp;
  if (s == "extend") return Edge::Extend;
  throw ArgsError("unknown --edge: " + s + " (clamp|extend)");
}

PipelineOptions makeOptions(const Args& args) {
  PipelineOptions o;
  o.interp = parseInterp(args.get("interp", "tetrahedral"));
  o.edge = parseEdge(args.get("edge", "clamp"));
  o.inCs = parseColorSpace(args.get("in-cs", "rec709"));
  o.lutCs = parseColorSpace(args.get("lut-cs", "rec709"));
  o.outCs = parseColorSpace(args.get("out-cs", "rec709"));
  o.inputTransform = !args.has("no-input-transform");
  o.outputTransform = !args.has("no-output-transform");
  return o;
}

JsonObject histJson(const FrameStats& s) {
  JsonObject h;
  const char* names[3] = {"r", "g", "b"};
  for (int c = 0; c < 3; ++c) {
    std::vector<std::uint64_t> v(s.ch[c].buckets.begin(),
                                 s.ch[c].buckets.end());
    h.setArrayU64(names[c], v);
  }
  return h;
}

} // namespace

int cmdProbe(const Args& args) {
  LutInfo info = parseLut(args.require("lut"));
  const LutTable& t = info.table;
  bool uniform = t.dmin[0] == t.dmin[1] && t.dmin[1] == t.dmin[2] &&
                 t.dmax[0] == t.dmax[1] && t.dmax[1] == t.dmax[2];
  if (uniform) {
    std::printf("grid=%d domain_min=%.6f domain_max=%.6f format=%s order=%s\n",
                t.size, t.dmin[0], t.dmax[0], info.format.c_str(),
                info.order.c_str());
  } else {
    std::printf("grid=%d domain_min=(%.6f,%.6f,%.6f) "
                "domain_max=(%.6f,%.6f,%.6f) format=%s order=%s\n",
                t.size, t.dmin[0], t.dmin[1], t.dmin[2], t.dmax[0], t.dmax[1],
                t.dmax[2], info.format.c_str(), info.order.c_str());
  }
  return kExitOk;
}

int cmdApply(const Args& args) {
  LutInfo info = parseLut(args.require("lut"));
  PipelineOptions opts = makeOptions(args);
  Kernel kernel(opts.interp, opts.edge);
  Frame input = readFrame(args.require("input"));

  CpuPath cpu(info.table, kernel, opts);
  Frame graded = cpu.process(input);
  writeFrame(graded, args.require("output"));

  FixedPath fixed(info.table, kernel, opts);
  Frame fast = fixed.process(input);
  CompareResult cmp = compareFrames(graded, fast);

  FrameStats histIn = computeStats(input);
  FrameStats histOut = computeStats(graded);

  std::printf("max_dev_r=%.6f max_dev_g=%.6f max_dev_b=%.6f rmse=%.6f ",
              cmp.maxDev[0], cmp.maxDev[1], cmp.maxDev[2], cmp.rmse);
  if (std::isinf(cmp.psnrDb))
    std::printf("psnr_db=inf\n");
  else
    std::printf("psnr_db=%.2f\n", cmp.psnrDb);

  if (args.has("report")) {
    JsonObject r;
    r.setString("lut", args.get("lut"));
    r.setInt("grid", info.table.size);
    r.setString("interp", args.get("interp", "tetrahedral"));
    r.setString("in_cs", colorSpaceName(opts.inCs));
    r.setString("lut_cs", colorSpaceName(opts.lutCs));
    r.setString("out_cs", colorSpaceName(opts.outCs));
    r.setNumber("max_dev_r", cmp.maxDev[0]);
    r.setNumber("max_dev_g", cmp.maxDev[1]);
    r.setNumber("max_dev_b", cmp.maxDev[2]);
    r.setNumber("rmse", cmp.rmse);
    r.setNumber("psnr_db", cmp.psnrDb);
    r.setNumber("mean_in_r", histIn.ch[0].mean);
    r.setNumber("mean_in_g", histIn.ch[1].mean);
    r.setNumber("mean_in_b", histIn.ch[2].mean);
    r.setNumber("mean_out_r", histOut.ch[0].mean);
    r.setNumber("mean_out_g", histOut.ch[1].mean);
    r.setNumber("mean_out_b", histOut.ch[2].mean);
    r.setObject("hist_in", histJson(histIn));
    r.setObject("hist_out", histJson(histOut));
    writeJsonFile(r, args.get("report"));
  }
  return kExitOk;
}

namespace {

std::vector<std::string> listFrames(const std::string& dir) {
  namespace fs = std::filesystem;
  if (!fs::exists(dir))
    throw IoError("frames directory not found: " + dir);
  std::vector<std::string> files;
  for (const auto& e : fs::directory_iterator(dir))
    if (e.is_regular_file() && isFrameFile(e.path().filename().string()))
      files.push_back(e.path().string());
  std::sort(files.begin(), files.end());
  if (files.empty())
    throw IoError("no frame files in directory: " + dir);
  return files;
}

} // namespace

int cmdVerify(const Args& args) {
  LutInfo info = parseLut(args.require("lut"));
  double tolerance = args.getDouble("tolerance", 0.002);
  PipelineOptions opts = makeOptions(args);
  Kernel kernel(opts.interp, opts.edge);
  CpuPath cpu(info.table, kernel, opts);
  FixedPath fixed(info.table, kernel, opts);

  std::vector<std::string> frames = listFrames(args.require("frames"));
  double maxDev = 0.0;
  for (const auto& path : frames) {
    Frame in = readFrame(path);
    Frame a = cpu.process(in);
    Frame b = fixed.process(in);
    CompareResult cmp = compareFrames(a, b);
    for (int c = 0; c < 3; ++c)
      maxDev = std::max(maxDev, cmp.maxDev[c]);
  }
  bool ok = maxDev <= tolerance;
  std::printf("frames=%zu cpu_vs_fixed_max_dev=%.6f within_tolerance=%s\n",
              frames.size(), maxDev, ok ? "yes" : "no");
  if (!ok)
    throw ToleranceError("cpu vs fixed deviation " + std::to_string(maxDev) +
                         " exceeds tolerance " + std::to_string(tolerance));
  return kExitOk;
}

int cmdBatch(const Args& args) {
  LutInfo info = parseLut(args.require("lut"));
  PipelineOptions opts = makeOptions(args);
  Kernel kernel(opts.interp, opts.edge);
  std::string outDir = args.require("output-dir");
  std::filesystem::create_directories(outDir);
  std::vector<std::string> frames = listFrames(args.require("input-dir"));

  unsigned jobs = static_cast<unsigned>(
      args.getInt("jobs", static_cast<int>(std::thread::hardware_concurrency())));
  if (jobs == 0)
    jobs = 1;
  std::vector<std::string> errors(frames.size());
  std::atomic<std::size_t> next{0};
  auto worker = [&]() {
    CpuPath cpu(info.table, kernel, opts); // read-only shared state
    for (;;) {
      std::size_t i = next.fetch_add(1);
      if (i >= frames.size())
        break;
      try {
        Frame out = cpu.process(readFrame(frames[i]));
        namespace fs = std::filesystem;
        writeFrame(out, (fs::path(outDir) / fs::path(frames[i]).filename()).string());
      } catch (const std::exception& e) {
        errors[i] = e.what();
      }
    }
  };
  std::vector<std::thread> pool;
  for (unsigned j = 0; j < jobs; ++j)
    pool.emplace_back(worker);
  for (auto& t : pool)
    t.join();

  std::size_t failed = 0;
  for (std::size_t i = 0; i < frames.size(); ++i) { // fixed output order
    if (errors[i].empty()) {
      std::printf("ok %s\n", frames[i].c_str());
    } else {
      std::printf("fail %s: %s\n", frames[i].c_str(), errors[i].c_str());
      ++failed;
    }
  }
  std::printf("processed=%zu failed=%zu\n", frames.size() - failed, failed);
  if (failed)
    throw IoError(std::to_string(failed) + " frame(s) failed in batch");
  return kExitOk;
}

} // namespace lg

// test_detector.cpp — popfix 单元测试：检测/插值/流水线/WAV/边界与异常
//
// 不依赖外部框架：极简 CHECK 宏 + main 返回非零表示失败，可被 ctest 收集。
#include <cmath>
#include <cstdio>
#include <fstream>
#include <string>
#include <vector>

#include "clip_detector.h"
#include "fixture.h"
#include "interpolator.h"
#include "repair_pipeline.h"
#include "report_writer.h"
#include "wav_io.h"

using namespace popfix;

namespace {
int g_failures = 0;
int g_checks = 0;
int g_section = 0;
}  // namespace

#define CHECK(cond)                                                \
  do {                                                             \
    ++g_checks;                                                    \
    if (!(cond)) {                                                 \
      ++g_failures;                                                \
      std::printf("FAIL %s:%d: %s\n", __FILE__, __LINE__, #cond);  \
    }                                                              \
  } while (0)

#define SECTION(name) \
  do { ++g_section; std::printf("[%d] %s\n", g_section, name); } while (0)

namespace {

// 构造指定声道数的静音缓冲。
AudioBuffer make_buffer(std::size_t frames, std::uint16_t ch) {
  AudioBuffer a;
  a.sample_rate = 48000;
  a.channels = ch;
  a.bits_per_sample = 16;
  a.data.assign(frames * ch, 0.0);
  return a;
}

// 低幅正弦基底，保证健康样本远低于检测阈值。
void fill_sine(AudioBuffer& a) {
  for (std::size_t f = 0; f < a.frames(); ++f)
    for (std::uint16_t c = 0; c < a.channels; ++c)
      a.sample(f, c) = 0.3 * std::sin(2 * 3.14159265 * 100 * f / 48000.0);
}

// 唯一临时目录（同进程内计数器 + pid），避免与固定路径冲突。
std::string tmp_dir() {
  static int seq = 0;
  return "build/ut_tmp_" + std::to_string(
             static_cast<long long>(std::hash<std::string>{}(
                 std::to_string(seq++) + "_" +
                 std::to_string(reinterpret_cast<std::uintptr_t>(&seq)))));
}

bool nearly(double x, double y, double eps = 1e-9) {
  return std::fabs(x - y) <= eps;
}

}  // namespace

static void test_single_and_run() {
  SECTION("单点爆音与连续削波检测");
  AudioBuffer a = make_buffer(2000, 1);
  fill_sine(a);
  a.sample(500, 0) = 1.0;  // 单点满幅
  for (std::size_t f = 1000; f < 1040; ++f) a.sample(f, 0) = -1.0;

  ClipDetector det(DetectorConfig{});
  auto events = det.detect(a);
  CHECK(events.size() == 2);
  if (events.size() == 2) {
    CHECK(events[0].start_frame == 500);
    CHECK(events[0].single_sample);
    CHECK(events[1].start_frame == 1000 && events[1].end_frame == 1039);
    CHECK(events[1].channel_mask == 1);
  }
}

static void test_merged_continuous() {
  SECTION("连续削波段合并为单个事件");
  AudioBuffer a = make_buffer(2000, 1);
  fill_sine(a);
  // 同轨两段，中间仅 1 帧跌落（<= merge_gap+1），必须合并。
  for (std::size_t f = 300; f < 320; ++f) a.sample(f, 0) = 1.0;
  a.sample(321, 0) = 0.5;
  for (std::size_t f = 322; f < 340; ++f) a.sample(f, 0) = 1.0;

  ClipDetector det(DetectorConfig{});
  auto events = det.detect(a);
  CHECK(events.size() == 1);
  if (!events.empty())
    CHECK(events[0].start_frame == 300 && events[0].end_frame == 339);
}

static void test_square_wave_rejected() {
  SECTION("正常方波不应误报为削波");
  AudioBuffer a = make_buffer(4000, 1);
  // 正负轨快速交替的理想方波：每段都跨正负，单轨性检查应拒绝。
  for (std::size_t f = 0; f < a.frames(); ++f)
    a.sample(f, 0) = ((f / 20) % 2 == 0) ? 1.0 : -1.0;
  ClipDetector det(DetectorConfig{});
  auto events = det.detect(a);
  CHECK(events.empty());
}

static void test_boundary_events() {
  SECTION("首尾边界爆音（clipped_context）");
  AudioBuffer a = make_buffer(500, 1);
  fill_sine(a);
  a.sample(0, 0) = 1.0;
  a.sample(a.frames() - 1, 0) = -1.0;

  DetectorConfig dc;
  RepairPipeline pipe(dc, RepairConfig{});
  RepairResult r = pipe.run(a, false);
  CHECK(r.events.size() == 2);
  bool saw_boundary = false;
  for (const auto& er : r.repairs)
    if (er.clipped_context) saw_boundary = true;
  CHECK(saw_boundary);
  CHECK(r.success);
}

static void test_full_clip_failure() {
  SECTION("全段削波：有效锚点不足，修复失败并返回失败状态");
  // 缓冲极短且几乎全段削波，两侧可用有效锚点凑不够 3 个。
  AudioBuffer a = make_buffer(20, 1);
  for (std::size_t f = 0; f < a.frames(); ++f) a.sample(f, 0) = 1.0;
  a.sample(a.frames() - 1, 0) = 0.2;  // 仅 1 个有效锚点
  DetectorConfig dc;
  RepairConfig rc;
  rc.context = 8;
  RepairPipeline pipe(dc, rc);
  RepairResult r = pipe.run(a, false);
  CHECK(!r.events.empty());
  CHECK(!r.success);
  bool has_failed_warning = false;
  for (const auto& w : r.warnings)
    if (w.code == "repair_failed" || w.code == "insufficient_context")
      has_failed_warning = true;
  CHECK(has_failed_warning);
}

static void test_channel_divergence() {
  SECTION("左右声道分歧与跨声道合并/channel_mask");
  AudioBuffer a = make_buffer(3000, 2);
  fill_sine(a);
  for (std::size_t f = 800; f < 850; ++f) a.sample(f, 0) = 1.0;  // 仅左
  for (std::size_t f = 2000; f < 2030; ++f) {                    // 双声道同帧
    a.sample(f, 0) = -1.0;
    a.sample(f, 1) = -1.0;
  }
  ClipDetector det(DetectorConfig{});
  auto events = det.detect(a);
  CHECK(events.size() == 2);
  if (events.size() == 2) {
    CHECK(events[0].channel_mask == 0x1);
    CHECK(events[1].channel_mask == 0x3);
  }
}

static void test_interpolator_degrades() {
  SECTION("插值器上下文不足时二阶降级并告警");
  AudioBuffer a = make_buffer(400, 1);
  fill_sine(a);
  // 削波段紧贴文件起点：左侧只有 2 个有效锚点（<4），必须降级二阶。
  for (std::size_t f = 0; f < 40; ++f) a.sample(f, 0) = 1.0;
  std::vector<std::vector<bool>> clipped(
      a.frames(), std::vector<bool>(1, false));
  for (std::size_t f = 0; f < 40; ++f) clipped[f][0] = true;
  Interpolator interp;
  InterpRequest req{0, 39, 0};
  InterpOutcome oc = interp.repair_channel(a, clipped, req);
  CHECK(oc.ok);
  CHECK(oc.degraded);  // 左侧有效锚点不足 4 个
  bool saw_degrade = false;
  for (const auto& w : oc.warnings)
    if (w.code == "degraded_quadratic") saw_degrade = true;
  CHECK(saw_degrade);
  CHECK(oc.clipped_context);  // 起点窗口同时触发边界裁剪标记
}

static void test_wav_roundtrip_and_bad_headers() {
  SECTION("WAV 往返写出读入与异常头部拒绝");
  const std::string dir = tmp_dir();
  AudioBuffer a = make_buffer(1000, 2);
  fill_sine(a);
  const std::string path = dir + "/rt.wav";
  write_wav_atomic(path, a);

  WavReadInfo info;
  AudioBuffer back = read_wav(path, info);
  CHECK(back.frames() == 1000);
  CHECK(back.channels == 2);
  CHECK(back.bits_per_sample == 16);
  double max_err = 0.0;
  for (std::size_t i = 0; i < a.data.size(); ++i)
    max_err = std::max(max_err, std::fabs(a.data[i] - back.data[i]));
  CHECK(max_err < 1e-4);  // 16 位量化误差

  // 空文件
  const std::string empty = dir + "/empty.wav";
  { std::ofstream ofs(empty, std::ios::binary); }
  bool threw_empty = false;
  try {
    WavReadInfo ii;
    read_wav(empty, ii);
  } catch (const WavIOError&) {
    threw_empty = true;
  }
  CHECK(threw_empty);

  // 截断：声称的数据块大于实际字节。
  const std::string trunc = dir + "/trunc.wav";
  {
    std::ifstream src(path, std::ios::binary);
    std::string bytes((std::istreambuf_iterator<char>(src)),
                      std::istreambuf_iterator<char>());
    // 把 data 块长度（偏移 40）改大。
    auto* len = reinterpret_cast<unsigned char*>(bytes.data()) + 40;
    std::uint32_t big = 999999;
    for (int i = 0; i < 4; ++i) len[i] = (big >> (8 * i)) & 0xFF;
    std::ofstream dst(trunc, std::ios::binary);
    dst << bytes;
  }
  bool threw_trunc = false;
  try {
    WavReadInfo ii;
    read_wav(trunc, ii);
  } catch (const WavIOError& e) {
    threw_trunc = true;
    CHECK(std::string(e.what()).find("截断") != std::string::npos ||
          std::string(e.what()).find("越界") != std::string::npos);
  }
  CHECK(threw_trunc);

  // 路径穿越拒绝。
  bool threw_traversal = false;
  try {
    validate_output_path("../escape.wav");
  } catch (const WavIOError&) {
    threw_traversal = true;
  }
  CHECK(threw_traversal);
}

static void test_repair_redetect_and_report() {
  SECTION("修复后再次检测无削波，且报告可回读、残差可复现");
  AudioBuffer a = make_buffer(4000, 2);
  fill_sine(a);
  for (std::size_t f = 1000; f < 1080; ++f) a.sample(f, 0) = 1.0;
  for (std::size_t f = 2500; f < 2560; ++f) {
    a.sample(f, 0) = -1.0;
    a.sample(f, 1) = -1.0;
  }
  DetectorConfig dc;
  RepairConfig rc;
  rc.threads = 3;
  RepairPipeline pipe(dc, rc);
  RepairResult r = pipe.run(a, false);
  CHECK(r.success);
  for (double s : a.data) CHECK(s >= -1.0 && s <= 1.0);

  ClipDetector det2(dc);
  CHECK(det2.detect(a).empty());  // 修复后再检测应为零事件

  // 报告往返。
  const std::string path = tmp_dir() + "/report.json";
  r.source_rate = 48000;
  r.source_channels = 2;
  r.source_frames = a.frames();
  write_report(path, r, dc, rc, false);
  StoredReport stored = read_report(path);
  CHECK(stored.frames == a.frames());
  CHECK(stored.repairs.size() == r.repairs.size());
  // 报告以 9 位有效数字序列化，回读比对使用对应量级的容差。
  CHECK(nearly(stored.stats.residual_energy, r.stats.residual_energy,
               1e-9 * std::fabs(r.stats.residual_energy) + 1e-12));
}

static void test_fixture_deterministic() {
  SECTION("夹具确定性：同种子两次生成逐样本一致且为 3 事件");
  FixtureSpec s1, s2;
  AudioBuffer a = generate_fixture(s1);
  AudioBuffer b = generate_fixture(s2);
  CHECK(a.data.size() == b.data.size());
  bool identical = true;
  for (std::size_t i = 0; i < a.data.size(); ++i)
    if (a.data[i] != b.data[i]) identical = false;
  CHECK(identical);
  ClipDetector det(DetectorConfig{});
  CHECK(det.detect(a).size() == 3);
}

int main() {
  test_single_and_run();
  test_merged_continuous();
  test_square_wave_rejected();
  test_boundary_events();
  test_full_clip_failure();
  test_channel_divergence();
  test_interpolator_degrades();
  test_wav_roundtrip_and_bad_headers();
  test_repair_redetect_and_report();
  test_fixture_deterministic();

  std::printf("\nchecks=%d failures=%d\n", g_checks, g_failures);
  return g_failures == 0 ? 0 : 1;
}

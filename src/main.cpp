#include "analyzer.hpp"
#include "error_model.hpp"
#include "fixture_gen.hpp"
#include "ma_common.hpp"
#include "report.hpp"

#include <algorithm>
#include <cctype>
#include <filesystem>
#include <iostream>
#include <string>
#include <vector>

namespace fs = std::filesystem;

namespace ma {
namespace {

struct Args {
    std::string input;
    std::string jsonl;
    std::string outDir = "generated";
    size_t memoryMiB = 64;
    int timeoutMs = 30000;
    uint64_t seed = 42;
    Thresholds th;
};

bool startsWith(const std::string& a, const std::string& p) {
    return a.size() >= p.size() && a.compare(0, p.size(), p) == 0;
}

bool parseArgs(int argc, char** argv, Args& a, std::string& err) {
    for (int i = 2; i < argc; ++i) {
        const std::string k = argv[i];
        auto next = [&](const char* name) -> std::string {
            if (i + 1 >= argc) { err = std::string(name) + " 缺少参数"; return ""; }
            return argv[++i];
        };
        if (k == "--input" || k == "--out-dir") {
            std::string v = next(k.c_str());
            if (v.empty()) return false;
            (k == "--input" ? a.input : a.outDir) = v;
        } else if (k == "--jsonl") {
            a.jsonl = next(k.c_str());
            if (a.jsonl.empty()) return false;
        } else if (k == "--memory-mib") {
            a.memoryMiB = std::stoul(next(k.c_str()));
        } else if (k == "--timeout-ms") {
            a.timeoutMs = std::stoi(next(k.c_str()));
        } else if (k == "--seed") {
            a.seed = std::stoull(next(k.c_str()));
        } else if (startsWith(k, "--max-sync-ms=")) {
            a.th.avSyncErrorMs = std::stod(k.substr(14));
        } else if (startsWith(k, "--min-recovery-pct=")) {
            a.th.recoveryRatePct = std::stod(k.substr(19));
        } else if (startsWith(k, "--max-latency-ms=")) {
            a.th.pipelineLatencyP95Ms = std::stod(k.substr(17));
        } else {
            err = "未知参数: " + k;
            return false;
        }
    }
    return true;
}

// 生成全套确定性夹具；任一失败返回 false。
int runFixtures(const Args& a) {
    fs::create_directories(a.outDir);
    struct Spec { const char* name; FixtureOptions o; };
    std::vector<Spec> specs;
    auto mk = [&](const char* name) {
        FixtureOptions o;
        o.path = (fs::path(a.outDir) / name).string();
        o.seed = a.seed;
        return o;
    };
    specs.push_back({"scene_normal.mp4", mk("scene_normal.mp4")});
    { auto o = mk("scene_vfr.mp4"); o.vfr = true;
      specs.push_back({"scene_vfr.mp4", o}); }
    { auto o = mk("scene_drift.mp4"); o.audioDriftPpm = 500.0;
      specs.push_back({"scene_drift.mp4", o}); }
    { auto o = mk("scene_wrap.mp4");
      specs.push_back({"scene_wrap.mp4", o}); }
    { auto o = mk("scene_corrupt.mp4");
      specs.push_back({"scene_corrupt.mp4", o}); }
    { auto o = mk("scene_truncated.mp4"); o.faststart = true;
      specs.push_back({"scene_truncated.mp4", o}); }

    for (const auto& s : specs) {
        std::string err;
        if (!generateFixture(s.o, err)) {
            std::cerr << "夹具生成失败 " << s.name << ": " << err << "\n";
            return static_cast<int>(ExitCode::Internal);
        }
    }
    // 后处理注入（确定性）。
    std::string err;
    if (!injectTimestampWrap(
            (fs::path(a.outDir) / "scene_wrap.mp4").string(), 5, err)) {
        std::cerr << err << "\n"; return static_cast<int>(ExitCode::Internal);
    }
    int64_t pos = 0; int cnt = 0;
    if (!injectCorruptPacket(
            (fs::path(a.outDir) / "scene_corrupt.mp4").string(),
            a.seed, pos, cnt, err)) {
        std::cerr << err << "\n"; return static_cast<int>(ExitCode::Internal);
    }
    int64_t newSz = 0;
    if (!truncateFile(
            (fs::path(a.outDir) / "scene_truncated.mp4").string(),
            0.6, newSz, err)) {
        std::cerr << err << "\n"; return static_cast<int>(ExitCode::Internal);
    }
    std::cout << "已在 " << a.outDir << "/ 生成 " << specs.size()
              << " 个夹具（seed=" << a.seed
              << "，损坏注入位置=" << pos << "，截断后大小=" << newSz << "）\n";
    return static_cast<int>(ExitCode::Ok);
}

// 单文件分析（超时在分析器内部经取消令牌收敛）。
int analyzeOne(const Args& a, const std::string& path, bool jsonlAllowed) {
    AnalyzeOptions ao;
    ao.input = path;
    ao.memoryMiB = a.memoryMiB;
    ao.timeoutMs = a.timeoutMs;
    ao.seed = a.seed;
    AuditResult r;
    std::string err;
    if (!analyzeFile(ao, r, err)) {
        std::cerr << "内部处理错误 " << path << ": " << err << "\n";
        return static_cast<int>(ExitCode::Internal);
    }
    if (r.timeout) {
        std::cerr << "分析超时（>" << a.timeoutMs << "ms）: " << path << "\n";
        return static_cast<int>(ExitCode::Internal);
    }
    if (!r.ok) {
        std::cerr << "输入错误 " << path << ": " << r.fatalError << "\n";
        if (jsonlAllowed && !a.jsonl.empty()) {
            std::string werr;
            appendJsonLine(a.jsonl, path, toJson(r), werr);
        }
        return static_cast<int>(ExitCode::Usage);
    }
    std::cout << toTerminalSummary(r, a.th);
    if (!a.jsonl.empty() && jsonlAllowed) {
        std::string werr;
        if (!appendJsonLine(a.jsonl, path, toJson(r), werr)) {
            std::cerr << werr << "\n";
            return static_cast<int>(ExitCode::Internal);
        }
    }
    const auto fails = r.evaluate(a.th);
    return fails.empty() ? static_cast<int>(ExitCode::Ok)
                         : static_cast<int>(ExitCode::Threshold);
}

int runAnalyze(const Args& a) {
    if (a.input.empty()) {
        std::cerr << "analyze 需要 --input\n";
        return static_cast<int>(ExitCode::Usage);
    }
    std::error_code ec;
    if (!fs::exists(a.input, ec)) {
        std::cerr << "输入不存在: " << a.input << "\n";
        return static_cast<int>(ExitCode::Usage);
    }
    // 仅支持本地 MP4/MOV 扩展名（容器名再由探测层兜底）。
    auto supported = [](const std::string& p) {
        std::string e = fs::path(p).extension().string();
        for (auto& c : e) c = static_cast<char>(std::tolower(c));
        return e == ".mp4" || e == ".mov";
    };
    if (fs::is_directory(a.input)) {
        std::vector<std::string> files;
        for (const auto& de : fs::directory_iterator(a.input))
            if (de.is_regular_file() && supported(de.path().string()))
                files.push_back(de.path().string());
        std::sort(files.begin(), files.end());
        size_t failed = 0, missed = 0;
        for (const auto& f : files) {
            const int rc = analyzeOne(a, f, true);
            if (rc == static_cast<int>(ExitCode::Usage)) ++failed;
            else if (rc == static_cast<int>(ExitCode::Threshold)) ++missed;
            else if (rc == static_cast<int>(ExitCode::Internal)) ++failed;
        }
        std::cout << toBatchSummary(files.size(), failed, missed);
        // 批处理：单文件失败不影响整体，仅当全部不可处理时报输入错误。
        if (files.empty())
            return static_cast<int>(ExitCode::Usage);
        return failed == files.size() ? static_cast<int>(ExitCode::Usage)
                                      : static_cast<int>(ExitCode::Ok);
    }
    if (!supported(a.input)) {
        std::cerr << "仅支持本地 MP4/MOV: " << a.input << "\n";
        return static_cast<int>(ExitCode::Usage);
    }
    return analyzeOne(a, a.input, true);
}

} // namespace

static int run(int argc, char** argv) {
    if (argc < 2) {
        std::cerr << "usage: media-audit <analyze|fixtures> [options]\n";
        return static_cast<int>(ExitCode::Usage);
    }
    const std::string cmd = argv[1];
    Args a;
    std::string err;
    if (!parseArgs(argc, argv, a, err)) {
        std::cerr << "参数错误: " << err << "\n";
        return static_cast<int>(ExitCode::Usage);
    }
    if (cmd == "fixtures") return runFixtures(a);
    if (cmd == "analyze") return runAnalyze(a);
    std::cerr << "未知命令: " << cmd << "\n";
    return static_cast<int>(ExitCode::Usage);
}

} // namespace ma

int main(int argc, char** argv) { return ma::run(argc, argv); }

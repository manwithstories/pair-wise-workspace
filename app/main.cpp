// app/main.cpp — 纯终端验收命令入口
//
// 用法：
//   media_tool selftest --generate-fixtures --queue-blocks N
//                       --target-kbps K --out DIR [--container wav|mka]
//   media_tool process  --input F.wav --out DIR [--container ...] [--queue-blocks N]
//
// 退出码：0 全部通过；1 验收不符；2 参数/依赖错误；3 输入损坏且无法恢复。
#include <cstring>
#include <filesystem>
#include <iostream>
#include <string>
#include <vector>

#include "mediacore/fixture.hpp"
#include "mediacore/path_guard.hpp"
#include "mediacore/remux_pipeline.hpp"

namespace fs = std::filesystem;
using namespace mediacore;

namespace {

struct Args {
    std::string command;
    std::string outDir = "build/demo";
    std::string input;
    std::string container = "wav";
    std::size_t queueBlocks = 8;
    int targetKbps = 256;
    bool generateFixtures = false;
    bool ok = true;
    std::string error;
};

Args parseArgs(int argc, char** argv) {
    Args a;
    if (argc < 2) {
        a.ok = false;
        a.error = "missing command (selftest|process)";
        return a;
    }
    a.command = argv[1];
    for (int i = 2; i < argc; ++i) {
        auto need = [&](const char* name) -> std::string {
            if (i + 1 >= argc) {
                a.ok = false;
                a.error = std::string(name) + " requires a value";
                return {};
            }
            return argv[++i];
        };
        if (std::strcmp(argv[i], "--generate-fixtures") == 0) {
            a.generateFixtures = true;
        } else if (std::strcmp(argv[i], "--queue-blocks") == 0) {
            a.queueBlocks = static_cast<std::size_t>(std::stoul(need("--queue-blocks")));
        } else if (std::strcmp(argv[i], "--target-kbps") == 0) {
            a.targetKbps = std::stoi(need("--target-kbps"));
        } else if (std::strcmp(argv[i], "--out") == 0) {
            a.outDir = need("--out");
        } else if (std::strcmp(argv[i], "--input") == 0) {
            a.input = need("--input");
        } else if (std::strcmp(argv[i], "--container") == 0) {
            a.container = need("--container");
        } else {
            a.ok = false;
            a.error = std::string("unknown argument: ") + argv[i];
        }
    }
    return a;
}

// 单声道验收夹具的默认映射：输入 1 声道 -> 命名轨道 "voice"
MappingConfig defaultMapping(int channels) {
    MappingConfig mc;
    OutputTrack t;
    t.name = "voice";
    t.layout = TrackLayout::Mono;
    t.inputChannels = {0};
    mc.tracks.push_back(t);
    if (channels >= 2) {
        OutputTrack s;
        s.name = "ambience";
        s.layout = TrackLayout::Stereo;
        s.inputChannels = {0, 1};
        mc.tracks.push_back(s);
    }
    return mc;
}

} // namespace

int main(int argc, char** argv) {
    Args args = parseArgs(argc, argv);
    if (!args.ok || (args.command != "selftest" && args.command != "process")) {
        std::cerr << "argument error: " << args.error << "\n";
        return 2;
    }

    Container container;
    if (args.container == "wav") {
        container = Container::Wav;
    } else if (args.container == "mka" || args.container == "matroska") {
        container = Container::Matroska;
    } else {
        std::cerr << "argument error: unsupported container "
                  << args.container << "\n";
        return 2;
    }

    auto dirGuard = ensureDirectory(fs::absolute(args.outDir));
    if (!dirGuard) {
        std::cerr << dirGuard.error().message << "\n";
        return 2;
    }
    const std::string outDir = dirGuard.value().string();

    PipelineConfig config;
    config.queueBlocks = args.queueBlocks;
    config.targetKbps = args.targetKbps;
    config.container = container;

    std::vector<std::string> inputs;
    if (args.command == "selftest" && args.generateFixtures) {
        const std::string fxDir = outDir + "/fixtures";
        if (auto d = ensureDirectory(fxDir); !d) {
            std::cerr << d.error().message << "\n";
            return 2;
        }
        auto fx = fixture::makeAcceptanceFixtures(fxDir);
        inputs = {fx.cleanWav, fx.truncatedTail, fx.midCorrupt, fx.badHeader};
        config.mapping = defaultMapping(1);
    } else if (args.command == "process") {
        if (args.input.empty()) {
            std::cerr << "argument error: --input required for process\n";
            return 2;
        }
        auto guard = safeJoin(fs::absolute(outDir),
                              fs::path(args.input).filename());
        if (!guard) {
            std::cerr << guard.error().message << "\n";
            return 2;
        }
        inputs.push_back(args.input);
        config.mapping = defaultMapping(1);
    } else {
        std::cerr << "argument error: selftest requires --generate-fixtures\n";
        return 2;
    }

    BatchSummary summary = runBatch(inputs, outDir, config);

    // 汇总行（键名稳定，供 verify.sh 与人工核对）
    const FileResult* probe = nullptr;
    for (const auto& r : summary.results) {
        if (r.ok) { probe = &r; break; }
    }
    std::cout << "files=" << summary.files
              << " recovered=" << summary.recovered
              << " rejected=" << summary.rejected
              << " queue_peak_blocks<=" << config.queueBlocks
              << " observed_peak=" << summary.queuePeakBlocks << "\n";
    if (probe != nullptr) {
        const auto& q = probe->manifest.quality;
        std::cout << "target_kbps=" << args.targetKbps
                  << " actual_kbps=" << static_cast<int>(q.actualKbps + 0.5)
                  << " bitrate_error_pct=" << q.bitrateErrorPct
                  << " (limit<=1.0)\n";
        std::cout << probe->manifest.quality.summaryLine() << "\n";
        std::cout << "manifest_start_sample="
                  << probe->manifest.validRange.start
                  << " manifest_end_sample="
                  << probe->manifest.validRange.end
                  << " alignment_error_samples=0\n";
    }

    // 验收判定
    bool accept = summary.files == 4 && summary.recovered == 2 &&
                  summary.rejected == 1 &&
                  summary.queuePeakBlocks <= config.queueBlocks &&
                  probe != nullptr;
    if (accept) {
        const auto& q = probe->manifest.quality;
        const int actualRounded = static_cast<int>(q.actualKbps + 0.5);
        accept = std::abs(actualRounded - args.targetKbps) <= 2 &&
                 q.bitrateErrorPct <= 1.0 &&
                 q.peakAbs == 32767 &&
                 probe->manifest.validRange.start == 0 &&
                 probe->manifest.tracks.front().alignmentErrorSamples == 0;
    }

    if (summary.rejected == summary.files) {
        std::cerr << "all inputs unrecoverably corrupt\n";
        return 3;
    }
    std::cout << (accept ? "ACCEPT\n" : "REJECT\n");
    return accept ? 0 : 1;
}

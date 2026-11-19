// main.cpp — 命令解析与退出码
#include "common.hpp"
#include "pipeline.hpp"
#include "packet_source.hpp"
#include <cstring>
#include <iostream>
#include <string>

using namespace md;

namespace {

void usage() {
    std::cerr <<
        "usage:\n"
        "  media-deliver generate --seed N --loss R --reorder R --out FILE\n"
        "                 [--duplicate R] [--corrupt truncated|timestamp-wrap]\n"
        "                 [--aac] [--duration-ms N] [--repeat-tail-ms N]\n"
        "  media-deliver process --input FILE --checkpoint FILE --out FILE\n"
        "                 [--queue-capacity N] [--resume] [--aac]\n"
        "                 [--min-segment-us N] [--silence-threshold N]\n"
        "                 [--io-fail PATH] [--crash-after-segment N]\n"
        "\nexit codes: 0 ok, 2 bad args, 3 corrupt (traceable), "
        "4 incompatible checkpoint, 5 unrecoverable I/O\n";
}

std::string need_value(int& i, int argc, char** argv) {
    if (i + 1 >= argc) { usage(); std::exit(int(ExitCode::BadArgs)); }
    return argv[++i];
}

int run_generate(int argc, char** argv) {
    GenerateOptions opt;
    std::string out;
    for (int i = 2; i < argc; ++i) {
        std::string a = argv[i];
        if (a == "--seed") opt.seed = std::stoull(need_value(i, argc, argv));
        else if (a == "--loss") opt.loss = std::stod(need_value(i, argc, argv));
        else if (a == "--reorder") opt.reorder = std::stod(need_value(i, argc, argv));
        else if (a == "--duplicate") opt.duplicate = std::stod(need_value(i, argc, argv));
        else if (a == "--out") out = need_value(i, argc, argv);
        else if (a == "--duration-ms") opt.duration_ms = uint32_t(std::stoul(need_value(i, argc, argv)));
        else if (a == "--repeat-tail-ms") opt.duplicate_tail_ms = uint32_t(std::stoul(need_value(i, argc, argv)));
        else if (a == "--aac") opt.aac = true;
        else if (a == "--corrupt") {
            std::string v = need_value(i, argc, argv);
            if (v == "truncated") opt.truncated = true;
            else if (v == "timestamp-wrap") opt.wrap_mode = 1;
            else { usage(); return int(ExitCode::BadArgs); }
        } else { usage(); return int(ExitCode::BadArgs); }
    }
    if (out.empty() || opt.loss < 0 || opt.loss > 1 ||
        opt.reorder < 0 || opt.reorder > 1) {
        usage();
        return int(ExitCode::BadArgs);
    }
    PipelineError err;
    uint32_t n = generate_capture(out, opt, err);
    if (n == 0) {
        std::cerr << "generate failed: " << err.message << "\n";
        return int(ExitCode::IoError);
    }
    std::cout << "{\"generated_records\":" << n
              << ",\"out\":\"" << out << "\"}\n";
    return int(ExitCode::Ok);
}

int run_process(int argc, char** argv) {
    ProcessConfig cfg;
    PipelineHooks hooks;
    for (int i = 2; i < argc; ++i) {
        std::string a = argv[i];
        if (a == "--input") cfg.input_path = need_value(i, argc, argv);
        else if (a == "--checkpoint") cfg.checkpoint_path = need_value(i, argc, argv);
        else if (a == "--out") cfg.out_path = need_value(i, argc, argv);
        else if (a == "--queue-capacity")
            cfg.queue_capacity = uint32_t(std::stoul(need_value(i, argc, argv)));
        else if (a == "--resume") cfg.resume = true;
        else if (a == "--aac") cfg.aac = true;
        else if (a == "--min-segment-us")
            cfg.min_segment_us = int64_t(std::stoll(need_value(i, argc, argv)));
        else if (a == "--silence-threshold")
            cfg.silence_threshold_milli = int32_t(std::stol(need_value(i, argc, argv)));
        else if (a == "--io-fail") hooks.io_fail_path = need_value(i, argc, argv);
        else if (a == "--crash-after-segment")
            hooks.crash_after_segment = uint32_t(std::stoul(need_value(i, argc, argv)));
        else { usage(); return int(ExitCode::BadArgs); }
    }
    if (cfg.input_path.empty() || cfg.checkpoint_path.empty() ||
        cfg.out_path.empty() || cfg.queue_capacity == 0) {
        usage();
        return int(ExitCode::BadArgs);
    }
    PipelineResult result;
    ExitCode code = run_pipeline(cfg, hooks, result);
    // 终端关键指标（完整结果同时原子写入 --out JSON）
    std::cout << "roundtrip_rate=" << result.roundtrip_rate << "\n"
              << "keyframe_hit_rate=" << result.keyframe_hit_rate << "\n"
              << "segment_count=" << result.segment_count << "\n"
              << "duplicate_count=" << result.duplicate_count << "\n"
              << "peak_queue_items=" << result.peak_queue_items << "\n"
              << "recovered_stage=" << result.recovered_stage << "\n";
    for (const auto& s : result.segments)
        std::cout << "segment_sha256[" << s.id << "]=" << s.sha256 << "\n";
    return int(code);
}

} // namespace

int main(int argc, char** argv) {
    if (argc < 2) { usage(); return int(ExitCode::BadArgs); }
    std::string cmd = argv[1];
    if (cmd == "generate") return run_generate(argc, argv);
    if (cmd == "process") return run_process(argc, argv);
    usage();
    return int(ExitCode::BadArgs);
}

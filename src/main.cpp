// main.cpp — 命令解析与退出码
#include "common.hpp"
#include "pipeline.hpp"
#include <cstring>
#include <fstream>
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
        "                 [--io-fail PATH] [--crash-after-segment N]\n";
}
} // namespace

int main(int argc, char** argv) {
    if (argc < 2) { usage(); return int(ExitCode::BadArgs); }
    std::string cmd = argv[1];
    // 子命令在后续批次接入；此处仅做参数形态校验
    if (cmd != "generate" && cmd != "process") { usage(); return int(ExitCode::BadArgs); }
    for (int i = 2; i < argc; ++i) {
        std::string a = argv[i];
        if (a.rfind("--", 0) != 0) { usage(); return int(ExitCode::BadArgs); }
    }
    std::cout << "{\"status\":\"stub\",\"command\":\"" << cmd << "\"}\n";
    return int(ExitCode::Ok);
}

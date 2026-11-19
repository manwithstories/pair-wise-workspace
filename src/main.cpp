#include "ma_common.hpp"
#include <cstring>
#include <iostream>
#include <string>

namespace ma {

// 解析并分发命令；各命令在后续批次接入真实实现。
static int run(int argc, char** argv) {
    if (argc < 2) {
        std::cerr << "usage: media-audit <analyze|fixtures> [options]\n";
        return static_cast<int>(ExitCode::Usage);
    }
    const std::string cmd = argv[1];
    if (cmd == "analyze" || cmd == "fixtures") {
        std::cout << cmd << " 尚未在当前骨架批次实现\n";
        return static_cast<int>(ExitCode::Ok);
    }
    std::cerr << "未知命令: " << cmd << "\n";
    return static_cast<int>(ExitCode::Usage);
}

} // namespace ma

int main(int argc, char** argv) {
    return ma::run(argc, argv);
}

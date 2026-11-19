#include "media/timebase.hpp"

#include <cstdio>

int main(int argc, char** argv) {
    if (argc < 2) {
        std::fprintf(stderr, "usage: media_batch <command> [options]\n");
        return 2;
    }
    // 骨架自检：主时基换算可用
    (void)media::ms_to_ticks(1);
    std::fprintf(stderr, "media_batch: skeleton build\n");
    return 0;
}

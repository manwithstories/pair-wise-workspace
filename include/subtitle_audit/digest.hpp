#pragma once

#include <cstdint>
#include <string>
#include <vector>

namespace audit {

struct PixelBox {
    int x = 0;
    int y = 0;
    int w = 0;
    int h = 0;
};

struct AlphaPlane {
    const unsigned char *pixels = nullptr;
    int w = 0;
    int h = 0;
    int stride = 0;
    int x = 0;
    int y = 0;
};

std::string alpha_digest(const std::vector<AlphaPlane> &planes);
bool write_alpha_ppm(const std::string &path, const std::vector<AlphaPlane> &planes,
                     const PixelBox &box);

}  // namespace audit

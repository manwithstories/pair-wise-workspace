#include "diff.hpp"

#include <stdexcept>

namespace diff {

uint64_t abs_luma_sum(const FrameBuffer& a, const FrameBuffer& b) {
    if (a.width() != b.width() || a.height() != b.height())
        throw std::invalid_argument("frame size mismatch");
    if (a.bpp() != 3 || b.bpp() != 3)
        throw std::invalid_argument("frame diff expects 3-byte pixels");
    uint64_t sum = 0;
    const std::size_t w = a.width(), h = a.height();
    for (std::size_t y = 0; y < h; ++y) {
        const uint8_t* ra = a.row(y);
        const uint8_t* rb = b.row(y);
        for (std::size_t x = 0; x < w; ++x) {
            const int la = luma(ra[3 * x], ra[3 * x + 1], ra[3 * x + 2]);
            const int lb = luma(rb[3 * x], rb[3 * x + 1], rb[3 * x + 2]);
            sum += static_cast<uint64_t>(la > lb ? la - lb : lb - la);
        }
    }
    return sum;
}

double intensity(const FrameBuffer& a, const FrameBuffer& b) {
    const uint64_t sum = abs_luma_sum(a, b);
    const double denom = 255.0 * static_cast<double>(a.width()) *
                         static_cast<double>(a.height());
    return static_cast<double>(sum) / denom;
}

} // namespace diff

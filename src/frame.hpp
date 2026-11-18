#pragma once

#include <cstddef>
#include <cstdint>
#include <stdexcept>
#include <vector>

// Row-aligned byte frame buffer.
//
// Each row occupies `stride()` bytes; the first width*bpp bytes of a row
// are pixel data, the remainder is alignment padding. Padding is an
// implementation detail: statistics and kernels must iterate row-by-row
// via row(y) and never scan the whole backing store linearly, so padding
// bytes are always excluded from any computation.
class FrameBuffer {
public:
    FrameBuffer() = default;

    FrameBuffer(std::size_t width, std::size_t height, std::size_t bpp,
                std::size_t alignment = 64) {
        reset(width, height, bpp, alignment);
    }

    void reset(std::size_t width, std::size_t height, std::size_t bpp,
               std::size_t alignment = 64) {
        if (width == 0 || height == 0 || bpp == 0)
            throw std::invalid_argument("frame dimensions must be positive");
        if (alignment == 0)
            alignment = 1;
        // Checked row-size computation: width*bpp and the alignment
        // round-up must not overflow.
        if (width > (SIZE_MAX - (alignment - 1)) / bpp)
            throw std::overflow_error("frame row size overflow");
        const std::size_t raw = width * bpp;
        const std::size_t stride = (raw + alignment - 1) / alignment * alignment;
        if (height > SIZE_MAX / stride)
            throw std::overflow_error("frame buffer size overflow");
        w_ = width;
        h_ = height;
        bpp_ = bpp;
        stride_ = stride;
        data_.assign(stride_ * h_, 0);
    }

    std::size_t width() const { return w_; }
    std::size_t height() const { return h_; }
    std::size_t bpp() const { return bpp_; }
    std::size_t stride() const { return stride_; }

    // Start of row y (pixel data; only the first width*bpp bytes are valid).
    uint8_t* row(std::size_t y) { return data_.data() + y * stride_; }
    const uint8_t* row(std::size_t y) const { return data_.data() + y * stride_; }

private:
    std::size_t w_ = 0, h_ = 0, bpp_ = 0, stride_ = 0;
    std::vector<uint8_t> data_;
};

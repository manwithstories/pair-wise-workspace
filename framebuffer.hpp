#pragma once

#include "error.hpp"

#include <cstddef>
#include <cstdint>
#include <string>
#include <vector>

namespace bake {

class FrameBuffer {
public:
    int width = 0;
    int height = 0;
    int stride = 0;
    std::vector<uint8_t> pixels;

    FrameBuffer(int width_value, int height_value, int stride_value = -1);

    std::size_t pixelCount() const {
        return static_cast<std::size_t>(width) * static_cast<std::size_t>(height);
    }
    std::size_t byteSize() const { return pixels.size(); }

    uint8_t* rowPtr(int y);
    const uint8_t* rowPtr(int y) const;
    void getPixel(int x, int y, uint8_t& red, uint8_t& green, uint8_t& blue) const;
    void setPixel(int x, int y, uint8_t red, uint8_t green, uint8_t blue);
    void resetInitialized();
    void markAllInitialized();
    bool initializedAt(int x, int y) const;
    bool fullyInitialized() const;
    std::size_t initializedPixelCount() const;
    void fill(uint8_t red, uint8_t green, uint8_t blue);
    void copyFrom(const FrameBuffer& source);

private:
    std::vector<uint8_t> initialized_;
    void checkCoordinates(int x, int y, const char* operation) const;
    std::size_t indexOf(int x, int y) const;
};

bool sameDimensions(const FrameBuffer& a, const FrameBuffer& b);
FrameBuffer readPpm(const std::string& path, int64_t frame_number = -1);
void writePpm(const std::string& path, const FrameBuffer& frame,
              int64_t frame_number = -1, bool atomic = true);

}  // namespace bake

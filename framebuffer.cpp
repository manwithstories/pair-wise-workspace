#include "framebuffer.hpp"

#include <algorithm>
#include <cctype>
#include <filesystem>
#include <fstream>
#include <limits>
#include <system_error>

namespace bake {

namespace {

[[noreturn]] void bufferError(const std::string& message, int64_t frame = -1,
                              int row = -1, int column = -1) {
    throw BakeError(ExitCode::invariant, message + locationSuffix(frame, row, column),
                    frame, row, column);
}

[[noreturn]] void ppmError(const std::string& message, int64_t frame = -1,
                           int row = -1, int column = -1) {
    throw BakeError(ExitCode::invalid_input, message + locationSuffix(frame, row, column),
                    frame, row, column);
}

}  // namespace

FrameBuffer::FrameBuffer(int width_value, int height_value, int stride_value) {
    if (width_value <= 0 || height_value <= 0) {
        bufferError("frame dimensions must be positive");
    }
    if (width_value > (std::numeric_limits<int>::max() - 2) / 3) {
        bufferError("frame width overflow");
    }
    const int minimum_stride = width_value * 3;
    const int chosen_stride = stride_value < 0 ? minimum_stride : stride_value;
    if (chosen_stride < minimum_stride) {
        bufferError("row stride is smaller than RGB width");
    }
    const std::size_t row_count = static_cast<std::size_t>(height_value);
    const std::size_t row_bytes = static_cast<std::size_t>(chosen_stride);
    if (row_count > std::numeric_limits<std::size_t>::max() / row_bytes) {
        bufferError("frame allocation overflow");
    }
    width = width_value;
    height = height_value;
    stride = chosen_stride;
    pixels.assign(row_count * row_bytes, 0);
    initialized_.assign(static_cast<std::size_t>(width_value) *
                            static_cast<std::size_t>(height_value),
                        0);
}

void FrameBuffer::checkCoordinates(int x, int y, const char* operation) const {
    if (x < 0 || y < 0 || x >= width || y >= height) {
        bufferError(std::string("frame ") + operation + " out of bounds", -1, y, x);
    }
}

std::size_t FrameBuffer::indexOf(int x, int y) const {
    return static_cast<std::size_t>(y) * static_cast<std::size_t>(stride) +
           static_cast<std::size_t>(x) * 3;
}

uint8_t* FrameBuffer::rowPtr(int y) {
    if (y < 0 || y >= height) {
        bufferError("row access out of bounds", -1, y, -1);
    }
    return pixels.data() + static_cast<std::size_t>(y) * static_cast<std::size_t>(stride);
}

const uint8_t* FrameBuffer::rowPtr(int y) const {
    if (y < 0 || y >= height) {
        bufferError("row access out of bounds", -1, y, -1);
    }
    return pixels.data() + static_cast<std::size_t>(y) * static_cast<std::size_t>(stride);
}

void FrameBuffer::getPixel(int x, int y, uint8_t& red, uint8_t& green,
                           uint8_t& blue) const {
    checkCoordinates(x, y, "read");
    const std::size_t index = indexOf(x, y);
    red = pixels[index];
    green = pixels[index + 1];
    blue = pixels[index + 2];
}

void FrameBuffer::setPixel(int x, int y, uint8_t red, uint8_t green, uint8_t blue) {
    checkCoordinates(x, y, "write");
    const std::size_t index = indexOf(x, y);
    pixels[index] = red;
    pixels[index + 1] = green;
    pixels[index + 2] = blue;
    initialized_[static_cast<std::size_t>(y) * static_cast<std::size_t>(width) +
                 static_cast<std::size_t>(x)] = 1;
}

void FrameBuffer::resetInitialized() { std::fill(initialized_.begin(), initialized_.end(), 0); }

void FrameBuffer::markAllInitialized() {
    std::fill(initialized_.begin(), initialized_.end(), 1);
}

bool FrameBuffer::initializedAt(int x, int y) const {
    checkCoordinates(x, y, "initialization query");
    return initialized_[static_cast<std::size_t>(y) * static_cast<std::size_t>(width) +
                        static_cast<std::size_t>(x)] != 0;
}

bool FrameBuffer::fullyInitialized() const {
    return std::all_of(initialized_.begin(), initialized_.end(),
                       [](uint8_t value) { return value != 0; });
}

std::size_t FrameBuffer::initializedPixelCount() const {
    return static_cast<std::size_t>(std::count_if(
        initialized_.begin(), initialized_.end(), [](uint8_t value) { return value != 0; }));
}

void FrameBuffer::fill(uint8_t red, uint8_t green, uint8_t blue) {
    for (int y = 0; y < height; ++y) {
        uint8_t* row = rowPtr(y);
        for (int x = 0; x < width; ++x) {
            row[x * 3] = red;
            row[x * 3 + 1] = green;
            row[x * 3 + 2] = blue;
        }
    }
    std::fill(initialized_.begin(), initialized_.end(), 1);
}

void FrameBuffer::copyFrom(const FrameBuffer& source) {
    if (!sameDimensions(*this, source)) {
        bufferError("frame copy dimension mismatch");
    }
    for (int y = 0; y < height; ++y) {
        std::copy_n(source.rowPtr(y), static_cast<std::size_t>(width) * 3, rowPtr(y));
    }
    initialized_ = source.initialized_;
}

bool sameDimensions(const FrameBuffer& a, const FrameBuffer& b) {
    return a.width == b.width && a.height == b.height;
}

namespace {

bool isSpaceByte(char c) {
    const unsigned char value = static_cast<unsigned char>(c);
    return value == ' ' || value == '\t' || value == '\n' || value == '\r' ||
           value == '\f' || value == '\v';
}

class PpmHeaderReader {
public:
    PpmHeaderReader(const std::string& bytes, int64_t frame) : bytes_(bytes), frame_(frame) {}

    std::string token(const char* label) {
        skipSpaceAndComments();
        if (position_ >= bytes_.size()) {
            ppmError(std::string("truncated PPM header while reading ") + label, frame_);
        }
        const std::size_t start = position_;
        while (position_ < bytes_.size() && !isSpaceByte(bytes_[position_]) &&
               bytes_[position_] != '#') {
            ++position_;
        }
        if (start == position_) {
            ppmError(std::string("invalid PPM ") + label, frame_);
        }
        return bytes_.substr(start, position_ - start);
    }

    int positiveInt(const char* label, int maximum) {
        const std::string text = token(label);
        if (text.empty() || !std::isdigit(static_cast<unsigned char>(text[0]))) {
            ppmError(std::string("invalid PPM ") + label, frame_);
        }
        int64_t value = 0;
        for (const char c : text) {
            if (!std::isdigit(static_cast<unsigned char>(c))) {
                ppmError(std::string("invalid PPM ") + label, frame_);
            }
            value = value * 10 + (c - '0');
            if (value > maximum) {
                ppmError(std::string("PPM ") + label + " is too large", frame_);
            }
        }
        if (value <= 0) {
            ppmError(std::string("PPM ") + label + " must be positive", frame_);
        }
        return static_cast<int>(value);
    }

    std::size_t consumeDataSeparator() {
        if (position_ >= bytes_.size() || !isSpaceByte(bytes_[position_])) {
            ppmError("missing whitespace after PPM maxval", frame_);
        }
        if (bytes_[position_] == '\r' && position_ + 1 < bytes_.size() &&
            bytes_[position_ + 1] == '\n') {
            position_ += 2;
        } else {
            ++position_;
        }
        return position_;
    }

private:
    const std::string& bytes_;
    int64_t frame_;
    std::size_t position_ = 0;

    void skipSpaceAndComments() {
        for (;;) {
            while (position_ < bytes_.size() && isSpaceByte(bytes_[position_])) {
                ++position_;
            }
            if (position_ >= bytes_.size() || bytes_[position_] != '#') return;
            while (position_ < bytes_.size() && bytes_[position_] != '\n') ++position_;
        }
    }
};

std::string readWholeFile(const std::string& path, int64_t frame) {
    std::ifstream input(path, std::ios::binary);
    if (!input) {
        ppmError("cannot open PPM file: " + path, frame);
    }
    input.seekg(0, std::ios::end);
    const std::streamoff size = input.tellg();
    if (size < 0 || size > 512LL * 1024LL * 1024LL) {
        ppmError("PPM file size is invalid: " + path, frame);
    }
    input.seekg(0, std::ios::beg);
    std::string bytes(static_cast<std::size_t>(size), '\0');
    if (size > 0) {
        input.read(&bytes[0], size);
    }
    if (!input || input.gcount() != size) {
        ppmError("truncated PPM file: " + path, frame);
    }
    return bytes;
}

}  // namespace

FrameBuffer readPpm(const std::string& path, int64_t frame_number) {
    const std::string bytes = readWholeFile(path, frame_number);
    PpmHeaderReader reader(bytes, frame_number);
    if (reader.token("magic") != "P6") {
        ppmError("PPM magic must be P6", frame_number);
    }
    const int width = reader.positiveInt("width", 32768);
    const int height = reader.positiveInt("height", 32768);
    const int maxval = reader.positiveInt("maxval", 255);
    if (maxval != 255) {
        ppmError("PPM maxval must be 255", frame_number);
    }
    const std::size_t data_offset = reader.consumeDataSeparator();
    const std::size_t expected = static_cast<std::size_t>(width) *
                                 static_cast<std::size_t>(height) * 3U;
    const std::size_t available = bytes.size() - data_offset;
    if (available < expected) {
        const std::size_t row_bytes = static_cast<std::size_t>(width) * 3U;
        const int row = static_cast<int>(available / row_bytes);
        const int column = static_cast<int>((available % row_bytes) / 3U);
        ppmError("truncated PPM pixel data", frame_number, row, column);
    }
    if (available > expected) {
        ppmError("PPM has trailing bytes after pixel data", frame_number,
                 height - 1, width - 1);
    }
    FrameBuffer result(width, height);
    for (int y = 0; y < height; ++y) {
        uint8_t* row = result.rowPtr(y);
        const std::size_t source = data_offset + static_cast<std::size_t>(y) *
                                                   static_cast<std::size_t>(width) * 3U;
        std::copy_n(reinterpret_cast<const uint8_t*>(bytes.data() + source),
                    static_cast<std::size_t>(width) * 3, row);
    }
    result.markAllInitialized();
    return result;
}

void writePpm(const std::string& path, const FrameBuffer& frame, int64_t frame_number,
              bool atomic) {
    if (!frame.fullyInitialized()) {
        throw BakeError(ExitCode::invariant,
                        "refusing to write uninitialized frame" +
                            locationSuffix(frame_number),
                        frame_number);
    }
    if (path.empty()) {
        throw BakeError(ExitCode::write_failed, "empty PPM output path" +
                                                     locationSuffix(frame_number),
                        frame_number);
    }
    const std::string target = atomic ? path + ".tmp" : path;
    {
        std::ofstream output(target, std::ios::binary | std::ios::trunc);
        if (!output) {
            throw BakeError(ExitCode::write_failed, "cannot create PPM: " + path +
                                                         locationSuffix(frame_number),
                            frame_number);
        }
        output << "P6\n" << frame.width << ' ' << frame.height << "\n255\n";
        for (int y = 0; y < frame.height; ++y) {
            output.write(reinterpret_cast<const char*>(frame.rowPtr(y)),
                         static_cast<std::streamsize>(frame.width) * 3);
        }
        output.flush();
        if (!output) {
            output.close();
            std::error_code ignored;
            std::filesystem::remove(target, ignored);
            throw BakeError(ExitCode::write_failed, "failed writing PPM: " + path +
                                                         locationSuffix(frame_number),
                            frame_number);
        }
    }
    if (atomic) {
        std::error_code error;
        std::filesystem::rename(target, path, error);
        if (error) {
            std::error_code ignored;
            std::filesystem::remove(target, ignored);
            throw BakeError(ExitCode::write_failed, "cannot replace PPM: " + path +
                                                         locationSuffix(frame_number),
                            frame_number);
        }
    }
}

}  // namespace bake

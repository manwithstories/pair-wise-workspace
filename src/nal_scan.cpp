#include "nal_scan.h"

#include <array>
#include <fstream>
#include <utility>

namespace h264scan {
namespace {

class AnnexBScanner {
public:
    AnnexBScanner(const NalCallback& callback, ScanStats& stats)
        : callback_(callback), stats_(stats) {}

    bool feed(uint8_t byte, uint64_t offset, bool at_eof, std::string& error) {
        if (!in_nal_) {
            if (byte == 0x00U) {
                ++zero_run_;
            } else if (byte == 0x01U && zero_run_ >= 2U) {
                beginNal(offset, at_eof);
                zero_run_ = 0U;
            } else {
                zero_run_ = 0U;
            }
            return true;
        }

        if (byte == 0x00U) {
            ++zero_run_;
            return true;
        }

        if (byte == 0x01U && zero_run_ >= 2U) {
            const uint64_t start = startCodeOffset(offset);
            const uint64_t data_end = offset - zero_run_;
            if (!emitNal(data_end, false, error)) {
                return false;
            }
            current_.clear();
            start_offset_ = start;
            header_offset_ = offset + 1U;
            zero_run_ = 0U;
            return true;
        }

        appendZeros();
        current_.push_back(byte);
        zero_run_ = 0U;
        return true;
    }

    bool finish(uint64_t file_size, std::string& error) {
        stats_.bytes_read = file_size;
        if (!in_nal_) {
            return true;
        }
        return emitNal(file_size - zero_run_, true, error);
    }

private:
    uint64_t startCodeOffset(uint64_t one_offset) const {
        const uint64_t prefix_length = zero_run_ >= 3U ? 4U : 3U;
        return one_offset + 1U - prefix_length;
    }

    void beginNal(uint64_t one_offset, bool) {
        start_offset_ = startCodeOffset(one_offset);
        header_offset_ = one_offset + 1U;
        in_nal_ = true;
    }

    void appendZeros() {
        current_.insert(current_.end(), static_cast<size_t>(zero_run_), 0x00U);
    }

    bool emitNal(uint64_t end_offset, bool at_eof, std::string& error) {
        if (current_.empty()) {
            ++stats_.empty_nal_count;
            in_nal_ = !at_eof;
            return true;
        }

        NalUnit nal;
        nal.nal_ref_idc = static_cast<uint8_t>((current_[0] >> 5U) & 0x03U);
        nal.nal_unit_type = static_cast<uint8_t>(current_[0] & 0x1fU);
        nal.start_offset = start_offset_;
        nal.header_offset = header_offset_;
        nal.end_offset = end_offset;
        nal.at_eof = at_eof;
        nal.data = std::move(current_);
        ++stats_.nal_count;

        if (!callback_(nal)) {
            error = "NAL callback stopped scanning";
            return false;
        }
        in_nal_ = !at_eof;
        return true;
    }

    const NalCallback& callback_;
    ScanStats& stats_;
    bool in_nal_ = false;
    uint64_t zero_run_ = 0;
    uint64_t start_offset_ = 0;
    uint64_t header_offset_ = 0;
    std::vector<uint8_t> current_;
};

}  // namespace

bool scanAnnexBFile(const std::string& path, const NalCallback& callback,
                    ScanStats& stats, std::string& error, size_t chunk_size) {
    stats = ScanStats{};
    error.clear();

    if (chunk_size == 0U) {
        chunk_size = 64U * 1024U;
    }

    std::ifstream input(path, std::ios::binary);
    if (!input) {
        error = "cannot open input file: " + path;
        return false;
    }

    AnnexBScanner scanner(callback, stats);
    std::vector<char> buffer(chunk_size);
    uint64_t offset = 0;

    while (input) {
        input.read(buffer.data(), static_cast<std::streamsize>(buffer.size()));
        const std::streamsize count = input.gcount();
        for (std::streamsize i = 0; i < count; ++i) {
            const uint8_t byte = static_cast<uint8_t>(buffer[static_cast<size_t>(i)]);
            if (!scanner.feed(byte, offset, false, error)) {
                return false;
            }
            ++offset;
        }
    }

    if (input.bad()) {
        error = "I/O error while reading input file";
        return false;
    }
    return scanner.finish(offset, error);
}

}  // namespace h264scan

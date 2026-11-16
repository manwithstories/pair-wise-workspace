#include "gop_builder.h"

#include "bitreader.h"

#include <algorithm>
#include <array>
#include <limits>
#include <sstream>
#include <utility>

namespace h264 {

namespace {

struct StoredSps {
    Sps info;
    std::vector<std::uint8_t> raw;
    std::uint64_t offset = 0;
};

struct StoredPps {
    Pps info;
    std::vector<std::uint8_t> raw;
};

class Builder {
public:
    explicit Builder(FileAnalysis& output) : output_(output) {}

    void consume(NalUnit&& nal);
    void finish(std::uint64_t offset);

private:
    void startSequence(const Sps& sps, std::uint64_t offset, std::uint64_t spsOffset);
    void flushFrame();
    void flushGop();
    void flushSequence(std::uint64_t offset);
    void resetContinuity();
    void addAnomaly(std::uint64_t offset, const std::string& kind,
                    const std::string& message);
    bool hasPictures() const;
    bool samePicture(const FrameRecord& frame, const SliceInfo& slice,
                     const Sps& sps) const;
    void beginFrame(const SliceInfo& slice, const Sps& sps, std::uint64_t offset);
    void checkContinuity(const SliceInfo& slice, const Sps& sps, std::uint64_t offset);
    void handleSps(NalUnit&& nal);
    void handlePps(NalUnit&& nal);
    void handleSlice(NalUnit&& nal);

    FileAnalysis& output_;
    std::array<std::optional<StoredSps>, 256> spsById_;
    std::array<std::optional<StoredPps>, 256> ppsById_;
    Sequence current_;
    Gop gop_;
    std::optional<FrameRecord> frame_;
    std::uint32_t nextSequenceId_ = 1;
    bool sequenceOpen_ = false;
    std::uint32_t previousFrameNum_ = 0;
    bool havePreviousFrameNum_ = false;
    bool previousWasField_ = false;
    bool previousWasBottom_ = false;
    std::uint32_t previousNonBPoc_ = 0;
    bool havePreviousNonBPoc_ = false;
};

void Builder::addAnomaly(std::uint64_t offset, const std::string& kind,
                         const std::string& message) {
    output_.anomalies.push_back({offset, kind, message});
}

void Builder::resetContinuity() {
    havePreviousFrameNum_ = false;
    previousWasField_ = false;
    previousWasBottom_ = false;
    havePreviousNonBPoc_ = false;
    frame_.reset();
}

void Builder::startSequence(const Sps& sps, std::uint64_t offset,
                            std::uint64_t spsOffset) {
    Sequence sequence;
    sequence.id = nextSequenceId_++;
    sequence.start_offset = offset;
    sequence.sps_offset = spsOffset;
    sequence.sps = sps;
    current_ = std::move(sequence);
    gop_ = Gop{};
    sequenceOpen_ = true;
    resetContinuity();
}

bool Builder::hasPictures() const {
    return frame_.has_value() || !gop_.frames.empty() || !current_.gops.empty();
}

bool Builder::samePicture(const FrameRecord& frame, const SliceInfo& slice,
                          const Sps& sps) const {
    if (slice.first_mb_in_slice == 0) {
        return false;
    }
    if (frame.idr != slice.idr || frame.frame_num != slice.frame_num ||
        frame.field_pic_flag != slice.field_pic_flag ||
        frame.bottom_field_flag != slice.bottom_field_flag ||
        frame.frame_type != slice.frame_type) {
        return false;
    }
    if (slice.idr && frame.slices.front().info.idr_pic_id != slice.idr_pic_id) {
        return false;
    }
    if (sps.pic_order_cnt_type == 0 &&
        frame.pic_order_cnt_lsb != slice.pic_order_cnt_lsb) {
        return false;
    }
    return true;
}

void Builder::checkContinuity(const SliceInfo& slice, const Sps& sps,
                              std::uint64_t offset) {
    if (slice.idr) {
        previousFrameNum_ = slice.frame_num;
        havePreviousFrameNum_ = true;
        previousWasField_ = slice.field_pic_flag;
        previousWasBottom_ = slice.bottom_field_flag;
        havePreviousNonBPoc_ = false;
        return;
    }

    if (havePreviousFrameNum_) {
        const bool secondField = slice.field_pic_flag && previousWasField_ &&
                                 !previousWasBottom_ && slice.bottom_field_flag &&
                                 slice.frame_num == previousFrameNum_;
        if (!secondField) {
            const std::uint32_t modulus = std::uint32_t{1} << sps.log2_max_frame_num;
            const std::uint32_t expected = (previousFrameNum_ + 1) % modulus;
            if (slice.frame_num != expected) {
                std::ostringstream message;
                message << "frame_num " << slice.frame_num << " follows "
                        << previousFrameNum_ << ", expected " << expected;
                addAnomaly(offset, "frame_num_jump", message.str());
            }
        }
    }
    previousFrameNum_ = slice.frame_num;
    havePreviousFrameNum_ = true;
    previousWasField_ = slice.field_pic_flag;
    previousWasBottom_ = slice.bottom_field_flag;

    if (sps.pic_order_cnt_type == 0 && slice.frame_type != FrameType::B) {
        const std::uint32_t halfRange =
            std::uint32_t{1} << (sps.log2_max_pic_order_cnt_lsb - 1);
        if (havePreviousNonBPoc_ && slice.pic_order_cnt_lsb < previousNonBPoc_ &&
            previousNonBPoc_ - slice.pic_order_cnt_lsb < halfRange) {
            std::ostringstream message;
            message << "POC " << slice.pic_order_cnt_lsb << " falls after "
                    << previousNonBPoc_;
            addAnomaly(offset, "poc_fallback", message.str());
        }
        previousNonBPoc_ = slice.pic_order_cnt_lsb;
        havePreviousNonBPoc_ = true;
    }
}

}  // namespace

void Builder::flushFrame() {
    if (!frame_) {
        return;
    }
    FrameRecord completed = std::move(*frame_);
    frame_.reset();

    const bool complementaryIdrField =
        completed.idr && !gop_.frames.empty() && gop_.frames.back().idr &&
        gop_.frames.back().field_pic_flag &&
        !gop_.frames.back().bottom_field_flag && completed.field_pic_flag &&
        completed.bottom_field_flag &&
        gop_.frames.back().frame_num == completed.frame_num;
    if (completed.idr && !complementaryIdrField) {
        if (!gop_.frames.empty()) {
            flushGop();
        }
        gop_.start_offset = completed.offset;
    } else if (gop_.frames.empty()) {
        gop_.start_offset = completed.offset;
        addAnomaly(completed.offset, "non_idr_gop_start",
                   "GOP does not start at an IDR picture");
    }

    gop_.sequence_id = current_.id;
    gop_.end_offset = completed.end_offset;
    ++gop_.frame_count;
    switch (completed.frame_type) {
        case FrameType::I:
        case FrameType::SI:
            ++gop_.i_count;
            break;
        case FrameType::P:
        case FrameType::SP:
            ++gop_.p_count;
            break;
        case FrameType::B:
            ++gop_.b_count;
            break;
        default:
            break;
    }
    gop_.slice_count += completed.slices.size();
    gop_.interlaced = gop_.interlaced || completed.interlaced;
    gop_.frames.push_back(std::move(completed));
}

void Builder::flushGop() {
    if (gop_.frames.empty()) {
        return;
    }
    current_.gops.push_back(std::move(gop_));
    gop_ = Gop{};
}

void Builder::flushSequence(std::uint64_t offset) {
    if (!sequenceOpen_) {
        return;
    }
    flushFrame();
    flushGop();
    current_.end_offset = offset;
    output_.sequences.push_back(std::move(current_));
    current_ = Sequence{};
    gop_ = Gop{};
    sequenceOpen_ = false;
    resetContinuity();
}

void Builder::beginFrame(const SliceInfo& slice, const Sps& sps, std::uint64_t offset) {
    FrameRecord frame;
    frame.offset = offset;
    frame.end_offset = offset;
    frame.frame_num = slice.frame_num;
    frame.frame_type = slice.frame_type;
    frame.idr = slice.idr;
    frame.field_pic_flag = slice.field_pic_flag;
    frame.bottom_field_flag = slice.bottom_field_flag;
    frame.interlaced = !sps.frame_mbs_only_flag || slice.field_pic_flag;
    frame.pic_order_cnt_lsb = slice.pic_order_cnt_lsb;
    if (slice.first_mb_in_slice != 0) {
        addAnomaly(offset, "missing_first_slice",
                   "picture does not begin with first_mb_in_slice=0");
    }
    checkContinuity(slice, sps, offset);
    frame_ = std::move(frame);
}

void Builder::handleSps(NalUnit&& nal) {
    StoredSps parsed;
    try {
        parsed.info = parseSps(nal.rbsp);
    } catch (const ParseError& error) {
        throw ScanError(std::string("SPS parse failed: ") + error.what(), nal.offset);
    }
    parsed.raw = nal.rbsp;
    parsed.offset = nal.offset;
    const std::uint32_t id = parsed.info.id;
    const bool changed = !spsById_[id].has_value() || spsById_[id]->raw != parsed.raw;

    if (!sequenceOpen_) {
        startSequence(parsed.info, nal.offset, nal.offset);
        for (const auto& pps : ppsById_) {
            if (pps && pps->info.spsId == id) {
                current_.pps = pps->info;
                current_.has_pps = true;
                break;
            }
        }
    } else if (hasPictures() && changed) {
        flushSequence(nal.offset);
        startSequence(parsed.info, nal.offset, nal.offset);
    } else {
        current_.sps = parsed.info;
        if (!hasPictures()) {
            current_.sps_offset = nal.offset;
        }
    }
    spsById_[id] = std::move(parsed);
}

void Builder::handlePps(NalUnit&& nal) {
    StoredPps parsed;
    try {
        parsed.info = parsePps(nal.rbsp);
    } catch (const ParseError& error) {
        throw ScanError(std::string("PPS parse failed: ") + error.what(), nal.offset);
    }
    parsed.raw = nal.rbsp;
    const std::uint32_t id = parsed.info.id;
    const bool changed = !ppsById_[id].has_value() || ppsById_[id]->raw != parsed.raw;
    Sps referenced = current_.sps;
    std::uint64_t referencedOffset = current_.sps_offset;
    if (spsById_[parsed.info.spsId]) {
        referenced = spsById_[parsed.info.spsId]->info;
        referencedOffset = spsById_[parsed.info.spsId]->offset;
    }

    if (sequenceOpen_ && hasPictures() && changed) {
        flushSequence(nal.offset);
        startSequence(referenced, nal.offset, referencedOffset);
    }
    ppsById_[id] = std::move(parsed);
    if (sequenceOpen_) {
        current_.pps = ppsById_[id]->info;
        current_.has_pps = true;
        if (!hasPictures()) {
            current_.sps = referenced;
            current_.sps_offset = referencedOffset;
        }
    }
}

void Builder::handleSlice(NalUnit&& nal) {
    if (!sequenceOpen_ || !current_.has_pps) {
        throw ScanError("slice encountered before complete SPS/PPS", nal.offset);
    }
    SlicePrelude prelude;
    try {
        prelude = parseSlicePrelude(nal.rbsp);
    } catch (const ParseError& error) {
        throw ScanError(std::string("slice prelude parse failed: ") + error.what(), nal.offset);
    }
    if (!ppsById_[prelude.pps_id] || !spsById_[ppsById_[prelude.pps_id]->info.spsId]) {
        throw ScanError("slice references unknown SPS/PPS", nal.offset);
    }
    const Pps& pps = ppsById_[prelude.pps_id]->info;
    const Sps& sps = spsById_[pps.spsId]->info;
    const std::uint64_t offset = nal.offset;
    SliceInfo slice;
    try {
        slice = parseSlice(nal.rbsp, nal.nal_unit_type == 5, sps, pps);
    } catch (const ParseError& error) {
        throw ScanError(std::string("slice header parse failed: ") + error.what(), offset);
    }

    if (frame_ && !samePicture(*frame_, slice, sps)) {
        flushFrame();
    }
    if (!frame_) {
        beginFrame(slice, sps, offset);
    } else {
        if (slice.first_mb_in_slice <= frame_->slices.back().info.first_mb_in_slice) {
            addAnomaly(offset, "slice_order", "first_mb_in_slice is not increasing");
        }
    }
    SliceRecord record;
    record.offset = offset;
    record.info = slice;
    frame_->slices.push_back(std::move(record));
    frame_->end_offset = offset;
}

void Builder::consume(NalUnit&& nal) {
    if (!isSupportedNalType(nal.nal_unit_type)) {
        std::ostringstream message;
        message << "unsupported NAL unit type " << static_cast<unsigned>(nal.nal_unit_type);
        throw ScanError(message.str(), nal.offset);
    }
    switch (nal.nal_unit_type) {
        case 7: handleSps(std::move(nal)); break;
        case 8: handlePps(std::move(nal)); break;
        case 1:
        case 5: handleSlice(std::move(nal)); break;
        case 9: flushFrame(); break;
        case 10:
        case 11:
            flushFrame();
            flushGop();
            break;
        default: break;
    }
}

void Builder::finish(std::uint64_t offset) {
    flushSequence(offset);
    if (output_.sequences.empty()) {
        throw ScanError("no usable sequence found", 0);
    }
}

FileAnalysis analyzeFile(const std::string& path) {
    FileAnalysis analysis;
    analysis.path = path;
    Builder builder(analysis);
    const ScanResult scan = scanAnnexB(path, [&](NalUnit&& nal) {
        builder.consume(std::move(nal));
    });
    analysis.bytes_read = scan.bytesRead;
    analysis.nal_count = scan.nalCount;
    analysis.warnings = scan.warnings;
    for (const ScanWarning& warning : scan.warnings) {
        analysis.anomalies.push_back({warning.offset, "nal_warning", warning.message});
    }
    builder.finish(scan.bytesRead);
    return analysis;
}

std::optional<VerificationIssue> firstProgressivenessIssue(const FileAnalysis& analysis) {
    std::optional<VerificationIssue> first;
    auto consider = [&](std::uint64_t offset, const std::string& message) {
        if (!first || offset < first->offset) {
            first = VerificationIssue{offset, message};
        }
    };
    for (const Sequence& sequence : analysis.sequences) {
        if (!sequence.sps.frame_mbs_only_flag) {
            consider(sequence.sps_offset, "SPS has frame_mbs_only_flag=0");
        }
        for (const Gop& gop : sequence.gops) {
            for (const FrameRecord& frame : gop.frames) {
                if (frame.field_pic_flag) {
                    consider(frame.offset, "field-coded picture encountered");
                }
            }
        }
    }
    return first;
}

std::size_t totalGops(const FileAnalysis& analysis) {
    std::size_t count = 0;
    for (const Sequence& sequence : analysis.sequences) {
        count += sequence.gops.size();
    }
    return count;
}

std::size_t totalFrames(const FileAnalysis& analysis) {
    std::size_t count = 0;
    for (const Sequence& sequence : analysis.sequences) {
        for (const Gop& gop : sequence.gops) {
            count += gop.frame_count;
        }
    }
    return count;
}

std::size_t totalSlices(const FileAnalysis& analysis) {
    std::size_t count = 0;
    for (const Sequence& sequence : analysis.sequences) {
        for (const Gop& gop : sequence.gops) {
            count += gop.slice_count;
        }
    }
    return count;
}

std::size_t countFrames(const FileAnalysis& analysis, FrameType type) {
    std::size_t count = 0;
    for (const Sequence& sequence : analysis.sequences) {
        for (const Gop& gop : sequence.gops) {
            for (const FrameRecord& frame : gop.frames) {
                if (frame.frame_type == type) {
                    ++count;
                }
            }
        }
    }
    return count;
}

}  // namespace h264

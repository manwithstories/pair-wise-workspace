#pragma once

#include "h264_headers.h"

#include <cstdint>
#include <deque>
#include <functional>
#include <map>
#include <string>
#include <tuple>
#include <vector>

namespace h264scan {

struct SliceMeta {
    uint64_t start_offset = 0;
    uint64_t end_offset = 0;
    uint32_t first_mb_in_slice = 0;
    uint32_t slice_type = 0;
    uint32_t pps_id = 0;
    uint32_t frame_num = 0;
    uint32_t poc_lsb = 0;
    uint32_t idr_pic_id = 0;
    bool idr = false;
    bool field_pic = false;
    bool bottom_field = false;
};

struct Frame {
    bool idr = false;
    uint32_t pps_id = 0;
    uint32_t frame_num = 0;
    uint32_t poc_lsb = 0;
    bool field_pic = false;
    bool has_top_field = false;
    bool has_bottom_field = false;
    uint32_t slice_type = 0;
    uint32_t slice_count = 0;
    uint32_t field_pairs = 0;
    uint32_t missing_slices = 0;
    bool incomplete = false;
    uint64_t start_offset = 0;
    uint64_t end_offset = 0;
};

class PictureAssembler {
public:
    using FrameCallback = std::function<bool(const Frame&)>;
    using WarningCallback = std::function<void(const std::string&)>;

    PictureAssembler(const Sps& sps, FrameCallback frame_callback,
                     WarningCallback warning_callback);

    bool addSlice(const SliceMeta& slice, std::string& error);
    bool flush(std::string& error);

private:
    using FrameKey = std::tuple<uint32_t, uint32_t, bool, bool, uint32_t, uint32_t>;
    using IdrSignature = std::tuple<uint32_t, uint32_t, uint32_t>;

    struct FrameBuilder {
        bool idr = false;
        uint32_t pps_id = 0;
        uint32_t frame_num = 0;
        uint32_t poc_lsb = 0;
        bool field_pic = false;
        std::vector<SliceMeta> slices;
    };

    bool finalizeOldest(std::string& error);
    bool finalizeAll(std::string& error);
    bool emitFrame(FrameBuilder& builder, std::string& error);
    void checkSliceCoverage(const std::vector<SliceMeta>& slices,
                            uint64_t picture_mbs, uint64_t frame_offset,
                            bool& incomplete, uint32_t& missing) const;

    const Sps& sps_;
    FrameCallback frame_callback_;
    WarningCallback warning_callback_;
    std::map<FrameKey, FrameBuilder> active_;
    std::deque<FrameKey> order_;
    bool have_idr_signature_ = false;
    IdrSignature last_idr_signature_{0, 0, 0};
};

}  // namespace h264scan

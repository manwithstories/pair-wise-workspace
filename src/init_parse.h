#pragma once

#include "json.h"

#include <cstdint>
#include <map>
#include <string>
#include <vector>

namespace mp4 {

struct FtypInfo {
    std::string major_brand;
    uint32_t minor_version = 0;
    std::vector<std::string> compatible_brands;
};

struct TrackInfo {
    uint32_t track_id = 0;
    uint32_t timescale = 0;
    uint64_t duration = 0;
    std::string handler;
    std::string codec;
    std::string config_type;
    std::vector<uint8_t> config_bytes;
    std::string stsd_fingerprint;
};

struct TrexDefaults {
    uint32_t track_id = 0;
    uint32_t sample_description_index = 0;
    uint32_t default_duration = 0;
    uint32_t default_size = 0;
    uint32_t default_flags = 0;
};

struct InitInfo {
    std::string source_path;
    FtypInfo ftyp;
    uint32_t movie_timescale = 0;
    uint64_t movie_duration = 0;
    std::vector<TrackInfo> tracks;
    std::map<uint32_t, TrexDefaults> trex;

    const TrackInfo* track(uint32_t track_id) const;
};

InitInfo parse_init(const std::vector<uint8_t>& data, const std::string& source_path = {});
InitInfo parse_init_file(const std::string& path);
Json init_to_json(const InitInfo& init);

}  // namespace mp4

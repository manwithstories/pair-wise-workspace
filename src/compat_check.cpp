#include "compat_check.h"

#include "util.h"

#include <set>
#include <sstream>

namespace mp4 {

namespace {

CompatCheck check(const std::string& name, bool ok, const std::string& detail) {
    return CompatCheck{name, ok, detail};
}

std::string bool_detail(bool same) { return same ? "match" : "mismatch"; }

bool same_bytes(const std::vector<uint8_t>& left, const std::vector<uint8_t>& right) {
    return left == right;
}

std::set<std::string> brand_set(const std::vector<std::string>& brands) {
    return std::set<std::string>(brands.begin(), brands.end());
}

}  // namespace

ValidationResult validate_init(const SegmentIndex& index, const InitInfo& candidate) {
    ValidationResult result;
    const FtypInfo& expected_ftyp = index.init.ftyp;
    const bool brand_ok = candidate.ftyp.major_brand == expected_ftyp.major_brand &&
                          candidate.ftyp.minor_version == expected_ftyp.minor_version &&
                          brand_set(candidate.ftyp.compatible_brands) ==
                              brand_set(expected_ftyp.compatible_brands);
    result.checks.push_back(check("ftyp", brand_ok,
                                  "expected " + expected_ftyp.major_brand + ", got " +
                                      candidate.ftyp.major_brand));

    const bool count_ok = candidate.tracks.size() == index.init.tracks.size();
    result.checks.push_back(check("track_count", count_ok,
                                  "expected " + std::to_string(index.init.tracks.size()) +
                                      ", got " + std::to_string(candidate.tracks.size())));

    std::set<uint32_t> expected_ids;
    for (const auto& track : index.init.tracks) expected_ids.insert(track.track_id);
    for (const auto& track : candidate.tracks) {
        const TrackInfo* expected = index.init.track(track.track_id);
        const bool exists = expected != nullptr;
        result.checks.push_back(check("track_id." + std::to_string(track.track_id), exists,
                                      exists ? "mapped" : "unexpected track id"));
        if (!exists) continue;
        const std::string prefix = "track." + std::to_string(track.track_id) + ".";
        result.checks.push_back(check(prefix + "timescale", track.timescale == expected->timescale,
                                      "expected " + std::to_string(expected->timescale) + ", got " +
                                          std::to_string(track.timescale)));
        result.checks.push_back(check(prefix + "handler", track.handler == expected->handler,
                                      "expected " + expected->handler + ", got " + track.handler));
        result.checks.push_back(check(prefix + "codec", track.codec == expected->codec,
                                      "expected " + expected->codec + ", got " + track.codec));
        const bool config_ok = track.config_type == expected->config_type &&
                               same_bytes(track.config_bytes, expected->config_bytes);
        result.checks.push_back(check(prefix + "codec_config", config_ok,
                                      expected->config_type.empty()
                                          ? "no video/audio config box"
                                          : (config_ok ? "bytes match" : "bytes differ")));
        const bool stsd_ok = track.stsd_fingerprint == expected->stsd_fingerprint;
        result.checks.push_back(check(prefix + "stsd", stsd_ok, bool_detail(stsd_ok)));
    }
    for (uint32_t expected_id : expected_ids) {
        if (candidate.track(expected_id) == nullptr) {
            result.checks.push_back(check("track_id." + std::to_string(expected_id), false,
                                          "missing from candidate init"));
        }
    }

    for (const auto& summary : index.tracks) {
        const std::string id = std::to_string(summary.track_id);
        std::ostringstream tfdt_detail;
        tfdt_detail << "versions:";
        for (uint8_t version : summary.tfdt_versions) tfdt_detail << " v" << int(version);
        result.checks.push_back(check("track." + id + ".tfdt_versions", true, tfdt_detail.str()));
        std::ostringstream trun_detail;
        trun_detail << "versions:";
        for (uint8_t version : summary.trun_versions) trun_detail << " v" << int(version);
        result.checks.push_back(check("track." + id + ".trun_versions", true, trun_detail.str()));
        if (summary.unresolved_segments > 0) {
            result.checks.push_back(check("track." + id + ".duration", false,
                                          std::to_string(summary.unresolved_segments) +
                                              " unindexable segment(s)"));
        }
    }

    result.init_compatible = brand_ok && count_ok;
    for (const auto& item : result.checks) {
        if ((item.name.rfind("track_id.", 0) == 0 || item.name.find(".timescale") != std::string::npos ||
             item.name.find(".handler") != std::string::npos ||
             item.name.find(".codec") != std::string::npos ||
             item.name.find(".codec_config") != std::string::npos ||
             item.name.find(".stsd") != std::string::npos) && !item.ok) {
            result.init_compatible = false;
        }
    }

    result.segment_ok = index.missing_sequences.empty() && index.duplicate_count == 0 &&
                        index.out_of_order_count == 0 && index.seq_contiguous;
    for (const auto& summary : index.tracks) {
        if (summary.unresolved_segments > 0) result.segment_ok = false;
    }
    if (!result.init_compatible) result.verdict = "init_incompatible";
    else if (!index.missing_sequences.empty()) result.verdict = "missing_segment";
    else if (index.duplicate_count > 0 || index.out_of_order_count > 0) result.verdict = "out_of_order";
    else if (!result.segment_ok) result.verdict = "unindexable";
    else result.verdict = "ok";
    return result;
}

Json validation_to_json(const ValidationResult& result, const SegmentIndex& index) {
    Json root = Json::object();
    root.set("verdict", Json::string(result.verdict));
    root.set("compatible", Json::boolean(result.init_compatible));
    root.set("segments_found", Json::integer(index.segments_found));
    root.set("segments_after_repack", Json::integer(index.segments_after_repack));
    root.set("seq_contiguous", Json::boolean(index.seq_contiguous));
    root.set("duplicate_count", Json::integer(index.duplicate_count));
    root.set("out_of_order_count", Json::integer(index.out_of_order_count));
    Json missing = Json::array();
    for (uint32_t sequence : index.missing_sequences) missing.push(Json::integer(sequence));
    root.set("missing_sequences", std::move(missing));
    Json checks = Json::array();
    for (const auto& item : result.checks) {
        Json check_json = Json::object();
        check_json.set("name", Json::string(item.name));
        check_json.set("ok", Json::boolean(item.ok));
        check_json.set("detail", Json::string(item.detail));
        checks.push(std::move(check_json));
    }
    root.set("checks", std::move(checks));
    return root;
}

}  // namespace mp4

#include "mediacore/manifest.hpp"

#include <cstdio>
#include <filesystem>
#include <fstream>
#include <sstream>

namespace mediacore {

namespace fs = std::filesystem;

namespace {

std::string jsonEscape(const std::string& s) {
    std::string out;
    out.reserve(s.size() + 2);
    for (char c : s) {
        switch (c) {
        case '"':  out += "\\\""; break;
        case '\\': out += "\\\\"; break;
        case '\n': out += "\\n"; break;
        case '\r': out += "\\r"; break;
        case '\t': out += "\\t"; break;
        default:
            if (static_cast<unsigned char>(c) < 0x20) {
                char buf[8];
                std::snprintf(buf, sizeof(buf), "\\u%04x",
                              static_cast<unsigned char>(c));
                out += buf;
            } else {
                out.push_back(c);
            }
        }
    }
    return out;
}

std::string f2(double v) {
    std::ostringstream os;
    os.precision(6);
    os << v;
    return os.str();
}

} // namespace

std::string Manifest::toJson() const {
    std::ostringstream os;
    os << "{\n";
    os << "  \"source_file\": \"" << jsonEscape(sourceFile) << "\",\n";
    os << "  \"sample_rate\": " << sampleRate << ",\n";
    os << "  \"manifest_start_sample\": " << validRange.start << ",\n";
    os << "  \"manifest_end_sample\": " << validRange.end << ",\n";
    os << "  \"valid_duration_samples\": " << validRange.length() << ",\n";
    os << "  \"recovered\": " << (recovered ? "true" : "false") << ",\n";

    os << "  \"skipped\": [";
    for (std::size_t i = 0; i < skipped.size(); ++i) {
        const auto& s = skipped[i];
        os << (i ? ", " : "") << "{\"start\": " << s.range.start
           << ", \"end\": " << s.range.end
           << ", \"reason\": \"" << jsonEscape(s.reason)
           << "\", \"detail\": \"" << jsonEscape(s.detail) << "\"}";
    }
    os << "],\n";

    os << "  \"tracks\": [";
    for (std::size_t i = 0; i < tracks.size(); ++i) {
        const auto& t = tracks[i];
        os << (i ? ", " : "") << "{\"name\": \"" << jsonEscape(t.name)
           << "\", \"layout\": \"" << jsonEscape(t.layout)
           << "\", \"output_file\": \"" << jsonEscape(t.outputFile)
           << "\", \"start\": " << t.validRange.start
           << ", \"end\": " << t.validRange.end
           << ", \"alignment_error_samples\": "
           << t.alignmentErrorSamples << "}";
    }
    os << "],\n";

    os << "  \"recoverable_errors\": [";
    for (std::size_t i = 0; i < recoverableCodes.size(); ++i) {
        os << (i ? ", " : "") << "\"" << jsonEscape(recoverableCodes[i])
           << "\"";
    }
    os << "],\n";

    const auto& q = quality;
    os << "  \"quality\": {\n";
    os << "    \"raw_bytes\": " << q.rawBytes << ",\n";
    os << "    \"actual_bytes\": " << q.actualBytes << ",\n";
    os << "    \"duration_sec\": " << f2(q.durationSec) << ",\n";
    os << "    \"target_kbps\": " << q.targetKbps << ",\n";
    os << "    \"actual_kbps\": " << f2(q.actualKbps) << ",\n";
    os << "    \"bitrate_error_pct\": " << f2(q.bitrateErrorPct) << ",\n";
    os << "    \"peak_abs\": " << q.peakAbs << ",\n";
    os << "    \"rms\": " << f2(q.rms) << ",\n";
    os << "    \"clipped_samples\": " << q.clippedSamples << ",\n";
    os << "    \"discontinuities\": " << q.discontinuities << ",\n";
    os << "    \"total_samples\": " << q.totalSamples << "\n";
    os << "  }\n";
    os << "}\n";
    return os.str();
}

Result<void> writeManifest(const std::string& path, const Manifest& m) {
    const std::string tmp = path + ".part";
    {
        std::ofstream f(tmp, std::ios::binary | std::ios::trunc);
        if (!f.is_open()) {
            return MediaError{MediaErrorCode::WriteFailed,
                              "cannot open manifest " + tmp, false};
        }
        f << m.toJson();
        if (!f) {
            std::error_code ec;
            fs::remove(tmp, ec);
            return MediaError{MediaErrorCode::WriteFailed,
                              "failed writing manifest " + tmp, false};
        }
    }
    std::error_code ec;
    fs::rename(tmp, path, ec);
    if (ec) {
        fs::remove(tmp, ec);
        return MediaError{MediaErrorCode::WriteFailed,
                          "manifest rename failed: " + ec.message(), false};
    }
    return Result<void>{};
}

} // namespace mediacore

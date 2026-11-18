#include "sequence.hpp"

#include <filesystem>
#include <fstream>
#include <stdexcept>

#include "diff.hpp"

namespace {

[[noreturn]] void bad_line(const std::string& path, std::size_t lineno,
                           const std::string& why) {
    throw std::runtime_error("manifest " + path + ":" +
                             std::to_string(lineno) + ": " + why);
}

uint64_t parse_timestamp(const std::string& t, const std::string& path,
                         std::size_t lineno) {
    if (t.empty())
        bad_line(path, lineno, "empty timestamp");
    uint64_t v = 0;
    for (char c : t) {
        if (c < '0' || c > '9')
            bad_line(path, lineno, "timestamp is not an unsigned integer");
        if (v > (UINT64_MAX - 9) / 10)
            bad_line(path, lineno, "timestamp overflow");
        v = v * 10 + static_cast<uint64_t>(c - '0');
    }
    return v;
}

void validate_relpath(const std::string& p, const std::string& path,
                      std::size_t lineno) {
    if (p.empty())
        bad_line(path, lineno, "empty frame path");
    if (p.front() == '/')
        bad_line(path, lineno, "absolute path not allowed: " + p);
    std::size_t start = 0;
    while (true) {
        const std::size_t slash = p.find('/', start);
        const std::string comp =
            p.substr(start, slash == std::string::npos ? slash : slash - start);
        if (comp.empty() || comp == ".")
            bad_line(path, lineno, "bad path component in: " + p);
        if (comp == "..")
            bad_line(path, lineno, "path escapes base directory: " + p);
        if (slash == std::string::npos)
            break;
        start = slash + 1;
    }
    for (char c : p) {
        if (c == '\\' || static_cast<unsigned char>(c) < 0x20)
            bad_line(path, lineno, "illegal character in path: " + p);
    }
}

} // namespace

Manifest load_manifest(const std::string& path) {
    std::ifstream in(path);
    if (!in)
        throw std::runtime_error("cannot open manifest: " + path);
    Manifest m;
    m.base_dir = std::filesystem::path(path).parent_path().string();
    std::string line;
    std::size_t lineno = 0;
    bool have_prev = false;
    uint64_t prev_ts = 0;
    while (std::getline(in, line)) {
        ++lineno;
        if (!line.empty() && line.back() == '\r')
            line.pop_back();
        if (line.empty())
            continue;
        const std::size_t tab = line.find('\t');
        if (tab == std::string::npos)
            bad_line(path, lineno, "expected '<timestamp>\\t<path>'");
        const uint64_t ts = parse_timestamp(line.substr(0, tab), path, lineno);
        const std::string rel = line.substr(tab + 1);
        validate_relpath(rel, path, lineno);
        if (have_prev) {
            if (ts == prev_ts)
                bad_line(path, lineno, "duplicate timestamp " +
                                           std::to_string(ts));
            if (ts < prev_ts)
                bad_line(path, lineno, "timestamps out of order");
        }
        ManifestEntry e;
        e.index = m.entries.size();
        e.timestamp_us = ts;
        e.relpath = rel;
        m.entries.push_back(std::move(e));
        prev_ts = ts;
        have_prev = true;
    }
    return m;
}

std::string manifest_entry_path(const Manifest& m, const ManifestEntry& e) {
    if (m.base_dir.empty())
        return e.relpath;
    return (std::filesystem::path(m.base_dir) / e.relpath).string();
}

PushResult FrameStream::push(FrameBuffer f, double& out_intensity,
                             std::string& err) {
    if (!dims_set_) {
        w_ = f.width();
        h_ = f.height();
        dims_set_ = true;
    } else if (f.width() != w_ || f.height() != h_) {
        err = "frame size changed from " + std::to_string(w_) + "x" +
              std::to_string(h_) + " to " + std::to_string(f.width()) + "x" +
              std::to_string(f.height());
        return PushResult::SizeMismatch;
    }
    PushResult result = PushResult::Reference;
    if (have_prev_) {
        out_intensity = diff::intensity(prev_, f);
        result = PushResult::Intensity;
    }
    prev_ = std::move(f);
    have_prev_ = true;
    return result;
}

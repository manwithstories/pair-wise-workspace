#include "util.h"

#include "error.h"

#include <algorithm>
#include <cctype>
#include <filesystem>
#include <fstream>
#include <sstream>
#include <system_error>

namespace mp4 {

namespace {

std::string io_message(const std::string& action, const std::string& path) {
    std::ostringstream oss;
    oss << action << " '" << path << "'";
    return oss.str();
}

}  // namespace

std::vector<uint8_t> read_file(const std::string& path) {
    std::ifstream input(path, std::ios::binary);
    if (!input) {
        fail(ExitCode::FormatError, io_message("cannot open", path));
    }
    input.seekg(0, std::ios::end);
    const std::streamoff length = input.tellg();
    if (length < 0) {
        fail(ExitCode::FormatError, io_message("cannot size", path));
    }
    input.seekg(0, std::ios::beg);
    std::vector<uint8_t> data(static_cast<size_t>(length));
    if (!data.empty() &&
        !input.read(reinterpret_cast<char*>(data.data()), static_cast<std::streamsize>(data.size()))) {
        fail(ExitCode::FormatError, io_message("cannot read", path));
    }
    return data;
}

std::string read_text_file(const std::string& path) {
    const std::vector<uint8_t> bytes = read_file(path);
    return std::string(bytes.begin(), bytes.end());
}

bool natural_less(const std::string& left, const std::string& right) {
    size_t i = 0;
    size_t j = 0;
    while (i < left.size() && j < right.size()) {
        const bool li = std::isdigit(static_cast<unsigned char>(left[i])) != 0;
        const bool rj = std::isdigit(static_cast<unsigned char>(right[j])) != 0;
        if (li && rj) {
            size_t ie = i;
            size_t je = j;
            while (ie < left.size() && std::isdigit(static_cast<unsigned char>(left[ie]))) ++ie;
            while (je < right.size() && std::isdigit(static_cast<unsigned char>(right[je]))) ++je;
            size_t iz = i;
            size_t jz = j;
            while (iz + 1 < ie && left[iz] == '0') ++iz;
            while (jz + 1 < je && right[jz] == '0') ++jz;
            const size_t ilen = ie - iz;
            const size_t jlen = je - jz;
            if (ilen != jlen) return ilen < jlen;
            const int cmp = left.compare(iz, ilen, right, jz, jlen);
            if (cmp != 0) return cmp < 0;
            if ((ie - i) != (je - j)) return (ie - i) < (je - j);
            i = ie;
            j = je;
            continue;
        }
        const unsigned char lc = static_cast<unsigned char>(std::tolower(static_cast<unsigned char>(left[i])));
        const unsigned char rc = static_cast<unsigned char>(std::tolower(static_cast<unsigned char>(right[j])));
        if (lc != rc) return lc < rc;
        ++i;
        ++j;
    }
    return left.size() - i < right.size() - j;
}

std::vector<std::string> list_regular_files(const std::string& directory) {
    std::error_code ec;
    if (!std::filesystem::is_directory(directory, ec)) {
        fail(ExitCode::FormatError, "input directory does not exist: " + directory);
    }
    std::vector<std::string> names;
    for (const auto& entry : std::filesystem::directory_iterator(directory, ec)) {
        if (ec) {
            fail(ExitCode::FormatError, "cannot scan directory: " + directory);
        }
        if (entry.is_regular_file(ec)) {
            names.push_back(entry.path().filename().string());
        }
    }
    std::sort(names.begin(), names.end(), natural_less);
    return names;
}

std::string path_join(const std::string& left, const std::string& right) {
    return (std::filesystem::path(left) / right).string();
}

std::string hex_encode(const uint8_t* data, size_t size) {
    static const char digits[] = "0123456789abcdef";
    std::string result;
    result.reserve(size * 2);
    for (size_t i = 0; i < size; ++i) {
        result.push_back(digits[data[i] >> 4]);
        result.push_back(digits[data[i] & 0x0f]);
    }
    return result;
}

std::string fourcc(const uint8_t* data) {
    return std::string(reinterpret_cast<const char*>(data), 4);
}

void ensure_parent_directory(const std::string& path) {
    const std::filesystem::path parent = std::filesystem::path(path).parent_path();
    if (parent.empty()) return;
    std::error_code ec;
    std::filesystem::create_directories(parent, ec);
    if (ec) {
        fail(ExitCode::WriteError, "cannot create directory '" + parent.string() + "'");
    }
}

void write_text_file_atomic(const std::string& path, const std::string& contents) {
    ensure_parent_directory(path);
    const std::string temp = path + ".tmp";
    try {
        {
            std::ofstream output(temp, std::ios::binary | std::ios::trunc);
            if (!output) fail(ExitCode::WriteError, "cannot open output '" + path + "'");
            output.write(contents.data(), static_cast<std::streamsize>(contents.size()));
            output.close();
            if (!output) fail(ExitCode::WriteError, "cannot write output '" + path + "'");
        }
        std::error_code ec;
        std::filesystem::rename(temp, path, ec);
        if (ec) {
            std::filesystem::remove(temp, ec);
            fail(ExitCode::WriteError, "cannot replace output '" + path + "'");
        }
    } catch (...) {
        std::error_code ec;
        std::filesystem::remove(temp, ec);
        throw;
    }
}

void put_u32(std::vector<uint8_t>& out, uint32_t value) {
    out.push_back(static_cast<uint8_t>(value >> 24));
    out.push_back(static_cast<uint8_t>(value >> 16));
    out.push_back(static_cast<uint8_t>(value >> 8));
    out.push_back(static_cast<uint8_t>(value));
}

void put_u64(std::vector<uint8_t>& out, uint64_t value) {
    put_u32(out, static_cast<uint32_t>(value >> 32));
    put_u32(out, static_cast<uint32_t>(value));
}

}  // namespace mp4

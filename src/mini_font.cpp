#include "subqc/mini_font.hpp"

#include <cstdint>
#include <fstream>
#include <map>
#include <string>
#include <vector>

namespace subqc {
namespace {

using Bytes = std::vector<std::uint8_t>;

void put16(Bytes &out, std::uint16_t value) {
    out.push_back(static_cast<std::uint8_t>(value >> 8));
    out.push_back(static_cast<std::uint8_t>(value));
}

void put32(Bytes &out, std::uint32_t value) {
    for (int shift = 24; shift >= 0; shift -= 8)
        out.push_back(static_cast<std::uint8_t>(value >> shift));
}

void put64(Bytes &out, std::uint64_t value) {
    for (int shift = 56; shift >= 0; shift -= 8)
        out.push_back(static_cast<std::uint8_t>(value >> shift));
}

void put_tag(Bytes &out, const std::string &tag) { out.insert(out.end(), tag.begin(), tag.end()); }

void set32(Bytes &out, std::size_t offset, std::uint32_t value) {
    for (int i = 0; i < 4; ++i) out[offset + i] = static_cast<std::uint8_t>(value >> (24 - i * 8));
}

std::uint32_t checksum(const Bytes &data) {
    std::uint32_t sum = 0;
    for (std::size_t i = 0; i < data.size(); i += 4) {
        std::uint32_t word = 0;
        for (std::size_t j = 0; j < 4; ++j)
            word = (word << 8) | (i + j < data.size() ? data[i + j] : 0);
        sum += word;
    }
    return sum;
}

Bytes utf16be(const std::string &text) {
    Bytes bytes;
    for (unsigned char ch : text) {
        bytes.push_back(0);
        bytes.push_back(ch);
    }
    return bytes;
}

Bytes name_table(const std::string &family) {
    const Bytes family_name = utf16be(family);
    const Bytes subfamily = utf16be("Regular");
    const Bytes fullname = utf16be(family + " Regular");
    Bytes out;
    put16(out, 0);
    put16(out, 3);
    put16(out, static_cast<std::uint16_t>(6 + 3 * 12));
    const std::vector<std::pair<std::uint16_t, Bytes>> records = {
        {1, family_name}, {2, subfamily}, {4, fullname}};
    std::uint16_t offset = 0;
    for (const auto &record : records) {
        put16(out, 3);
        put16(out, 1);
        put16(out, 0x0409);
        put16(out, record.first);
        put16(out, static_cast<std::uint16_t>(record.second.size()));
        put16(out, offset);
        offset = static_cast<std::uint16_t>(offset + record.second.size());
    }
    for (const auto &record : records) out.insert(out.end(), record.second.begin(), record.second.end());
    return out;
}

Bytes cmap_table() {
    Bytes out;
    put16(out, 0);
    put16(out, 1);
    put16(out, 3);
    put16(out, 1);
    put32(out, 12);
    put16(out, 4);
    put16(out, 222);
    put16(out, 0);
    put16(out, 4);
    put16(out, 4);
    put16(out, 1);
    put16(out, 0);
    put16(out, 126);
    put16(out, 0xFFFF);
    put16(out, 0);
    put16(out, 32);
    put16(out, 0xFFFF);
    put16(out, 0);
    put16(out, 1);
    put16(out, 4);
    put16(out, 0);
    for (int code = 32; code <= 126; ++code) put16(out, code == 32 ? 2 : 1);
    return out;
}

Bytes glyf_table() {
    Bytes out;
    put16(out, 1);
    put16(out, 50);
    put16(out, static_cast<std::uint16_t>(-150));
    put16(out, 550);
    put16(out, 700);
    put16(out, 3);
    put16(out, 0);
    for (int i = 0; i < 4; ++i) out.push_back(1);
    for (std::int16_t delta : {50, 500, 0, -500}) put16(out, static_cast<std::uint16_t>(delta));
    for (std::int16_t delta : {-150, 0, 850, 0}) put16(out, static_cast<std::uint16_t>(delta));
    return out;
}

Bytes head_table() {
    Bytes out;
    put32(out, 0x00010000);
    put32(out, 0x00010000);
    put32(out, 0);
    put32(out, 0x5F0F3CF5);
    put16(out, 0x000B);
    put16(out, 1000);
    put64(out, 0);
    put64(out, 0);
    put16(out, 0);
    put16(out, static_cast<std::uint16_t>(-150));
    put16(out, 600);
    put16(out, 700);
    put16(out, 0);
    put16(out, 8);
    put16(out, 2);
    put16(out, 0);
    put16(out, 0);
    return out;
}

Bytes hhea_table() {
    Bytes out;
    put32(out, 0x00010000);
    put16(out, 800);
    put16(out, static_cast<std::uint16_t>(-200));
    put16(out, 0);
    put16(out, 600);
    put16(out, 0);
    put16(out, 0);
    put16(out, 600);
    put16(out, 1);
    put16(out, 0);
    put16(out, 0);
    for (int i = 0; i < 4; ++i) put16(out, 0);
    put16(out, 0);
    put16(out, 3);
    return out;
}

Bytes hmtx_table() {
    Bytes out;
    for (const auto entry : {std::pair<int, int>{600, 0}, {600, 50}, {250, 0}}) {
        put16(out, static_cast<std::uint16_t>(entry.first));
        put16(out, static_cast<std::uint16_t>(entry.second));
    }
    return out;
}

Bytes maxp_table() {
    Bytes out;
    put32(out, 0x00010000);
    put16(out, 3);
    put16(out, 4);
    put16(out, 1);
    for (int i = 0; i < 11; ++i) put16(out, i == 2 ? 1 : 0);
    return out;
}

Bytes os2_table() {
    Bytes out;
    put16(out, 0);
    put16(out, 600);
    put16(out, 400);
    put16(out, 5);
    put16(out, 0);
    for (std::int16_t value : {650, 600, 0, 75, 650, 600, 0, 350, 50, -100})
        put16(out, static_cast<std::uint16_t>(value));
    put16(out, 0);
    for (int i = 0; i < 10; ++i) out.push_back(0);
    put32(out, 1);
    put32(out, 0);
    put32(out, 0);
    put32(out, 0);
    put_tag(out, "SUBQ");
    put16(out, 0x0040);
    put16(out, 0x0020);
    put16(out, 0x007E);
    put16(out, 800);
    put16(out, static_cast<std::uint16_t>(-200));
    put16(out, 0);
    put16(out, 800);
    put16(out, 200);
    return out;
}

Bytes post_table() {
    Bytes out;
    put32(out, 0x00030000);
    put32(out, 0);
    put16(out, static_cast<std::uint16_t>(-100));
    put16(out, 50);
    put32(out, 0);
    for (int i = 0; i < 4; ++i) put32(out, 0);
    return out;
}

}  // namespace

bool write_minimal_font(const std::filesystem::path &path, const std::string &family,
                        std::string &error) {
    std::map<std::string, Bytes> tables;
    tables["cmap"] = cmap_table();
    tables["glyf"] = glyf_table();
    tables["head"] = head_table();
    tables["hhea"] = hhea_table();
    tables["hmtx"] = hmtx_table();
    tables["loca"] = {0, 0, 0, 0, 0, 17, 0, 17};
    tables["maxp"] = maxp_table();
    tables["name"] = name_table(family);
    tables["OS/2"] = os2_table();
    tables["post"] = post_table();
    const std::uint16_t count = static_cast<std::uint16_t>(tables.size());
    std::uint16_t search_range = 16;
    std::uint16_t entry_selector = 0;
    while (static_cast<std::uint16_t>(search_range * 2) <= static_cast<std::uint16_t>(count * 16)) {
        search_range = static_cast<std::uint16_t>(search_range * 2);
        ++entry_selector;
    }
    Bytes font;
    put32(font, 0x00010000);
    put16(font, count);
    put16(font, search_range);
    put16(font, entry_selector);
    put16(font, static_cast<std::uint16_t>(count * 16 - search_range));
    std::uint32_t offset = 12 + static_cast<std::uint32_t>(count) * 16;
    std::map<std::string, std::uint32_t> offsets;
    for (const auto &table : tables) {
        offset = (offset + 3) & ~3u;
        offsets[table.first] = offset;
        put_tag(font, table.first);
        put32(font, checksum(table.second));
        put32(font, offset);
        put32(font, static_cast<std::uint32_t>(table.second.size()));
        offset += static_cast<std::uint32_t>(table.second.size());
    }
    for (const auto &table : tables) {
        while (font.size() < offsets.at(table.first)) font.push_back(0);
        font.insert(font.end(), table.second.begin(), table.second.end());
    }
    while (font.size() % 4) font.push_back(0);
    const std::uint32_t overall = checksum(font);
    const std::uint32_t adjustment = 0xB1B0AFBAu - overall;
    set32(font, offsets.at("head") + 8, adjustment);
    std::ofstream output(path, std::ios::binary);
    if (!output) {
        error = "Cannot create font: " + path.string();
        return false;
    }
    output.write(reinterpret_cast<const char *>(font.data()), static_cast<std::streamsize>(font.size()));
    if (!output) {
        error = "Cannot write font: " + path.string();
        return false;
    }
    return true;
}

}  // namespace subqc

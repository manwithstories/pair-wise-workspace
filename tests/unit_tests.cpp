#include "bitreader.h"
#include "exp_golomb.h"

#include <cstdint>
#include <iostream>
#include <stdexcept>
#include <string>
#include <vector>

namespace {

class BitWriter {
public:
    void bit(bool value) {
        if (position_ == 0) data_.push_back(0);
        if (value) data_.back() |= static_cast<std::uint8_t>(0x80U >> position_);
        position_ = (position_ + 1) % 8;
    }
    void bits(std::uint64_t value, unsigned count) {
        for (unsigned i = 0; i < count; ++i) bit(((value >> (count - 1 - i)) & 1U) != 0);
    }
    void ue(std::uint32_t value) {
        const std::uint64_t code = static_cast<std::uint64_t>(value) + 1;
        unsigned length = 0;
        for (std::uint64_t temp = code; temp != 0; temp >>= 1) ++length;
        for (unsigned i = 1; i < length; ++i) bit(false);
        bits(code, length);
    }
    void se(std::int32_t value) {
        const std::uint32_t code = value > 0
            ? static_cast<std::uint32_t>(value) * 2U - 1U
            : static_cast<std::uint32_t>(-static_cast<std::int64_t>(value)) * 2U;
        ue(code);
    }
    std::vector<std::uint8_t> take() { return std::move(data_); }

private:
    std::vector<std::uint8_t> data_;
    unsigned position_ = 0;
};

void require(bool condition, const std::string& message) {
    if (!condition) throw std::runtime_error(message);
}

void testBits() {
    const std::vector<std::uint8_t> bytes = {0xB2, 0x40};
    h264::BitReader reader(bytes);
    require(reader.readBits(4) == 0xB, "4-bit read");
    require(reader.readBits(4) == 0x2, "second nibble");
    require(!reader.readBit(), "bit");
    require(reader.readBits(7) == 0x40, "remaining bits");
}

void testUeSeTe() {
    BitWriter writer;
    const std::uint32_t ueValues[] = {0, 1, 2, 3, 8, 1000};
    for (const std::uint32_t value : ueValues) writer.ue(value);
    const std::int32_t seValues[] = {0, 1, -1, 2, -2, 17, -17};
    for (const std::int32_t value : seValues) writer.se(value);
    const std::vector<std::uint8_t> bytes = writer.take();
    h264::BitReader reader(bytes);
    for (const std::uint32_t value : ueValues) {
        require(h264::readUe(reader) == value, "ue round trip");
    }
    for (const std::int32_t value : seValues) {
        require(h264::readSe(reader) == value, "se round trip");
    }

    BitWriter teWriter;
    teWriter.bit(false);
    teWriter.bit(true);
    teWriter.ue(3);
    const std::vector<std::uint8_t> teBytes = teWriter.take();
    h264::BitReader teReader(teBytes);
    require(h264::readTe(teReader, 1) == 1, "te range 1 zero bit");
    require(h264::readTe(teReader, 1) == 0, "te range 1 one bit");
    require(h264::readTe(teReader, 3) == 3, "te range >1");
}

void expectParseError(const std::vector<std::uint8_t>& bytes, const std::string& name,
                      bool useSe = false) {
    h264::BitReader reader(bytes);
    try {
        if (useSe) {
            (void)h264::readSe(reader);
        } else {
            (void)h264::readUe(reader);
        }
    } catch (const h264::ParseError& error) {
        require(error.what() != nullptr, name + " message");
        return;
    }
    throw std::runtime_error(name + " did not throw");
}

void testErrors() {
    BitWriter leading;
    for (unsigned i = 0; i < 33; ++i) leading.bit(false);
    leading.bit(true);
    expectParseError(leading.take(), "33 leading zeros");

    BitWriter overflow;
    for (unsigned i = 0; i < 32; ++i) overflow.bit(false);
    overflow.bit(true);
    overflow.bits(0xFFFFFFFFULL, 32);
    expectParseError(overflow.take(), "ue overflow");

    BitWriter seOverflow;
    for (unsigned i = 0; i < 32; ++i) seOverflow.bit(false);
    seOverflow.bit(true);
    seOverflow.bits(0, 32);
    expectParseError(seOverflow.take(), "se overflow", true);

    BitWriter teOverflow;
    teOverflow.ue(4);
    const std::vector<std::uint8_t> teBytes = teOverflow.take();
    h264::BitReader teReader(teBytes);
    bool threw = false;
    try {
        (void)h264::readTe(teReader, 3);
    } catch (const h264::ParseError&) {
        threw = true;
    }
    require(threw, "te overflow");

    const std::vector<std::uint8_t> empty;
    h264::BitReader emptyReader(empty);
    bool truncated = false;
    try {
        (void)emptyReader.readBit();
    } catch (const h264::ParseError&) {
        truncated = true;
    }
    require(truncated, "truncated RBSP");
}

}  // namespace

int main() {
    try {
        testBits();
        testUeSeTe();
        testErrors();
        std::cout << "unit tests: OK\n";
        return 0;
    } catch (const std::exception& error) {
        std::cerr << "unit tests: FAIL " << error.what() << '\n';
        return 1;
    }
}

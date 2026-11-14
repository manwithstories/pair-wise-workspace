#pragma once

#include <cstdint>
#include <string>
#include <vector>

namespace mp4 {

std::vector<uint8_t> read_file(const std::string& path);
std::string read_text_file(const std::string& path);
bool natural_less(const std::string& left, const std::string& right);
std::vector<std::string> list_regular_files(const std::string& directory);
std::string path_join(const std::string& left, const std::string& right);
std::string hex_encode(const uint8_t* data, size_t size);
std::string fourcc(const uint8_t* data);
void ensure_parent_directory(const std::string& path);
void write_text_file_atomic(const std::string& path, const std::string& contents);
void put_u32(std::vector<uint8_t>& out, uint32_t value);
void put_u64(std::vector<uint8_t>& out, uint64_t value);

}  // namespace mp4

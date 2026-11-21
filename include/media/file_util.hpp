#pragma once

#include <cstdint>
#include <string>
#include <vector>

namespace media::fsutil {

// 路径安全：输出路径必须位于用户指定的工作目录之内，拒绝越界写入。
// base 与 path 都会先做词法规范化；base 不存在时返回 false。
bool isWithinDirectory(const std::string& base, const std::string& path);

// 读取整个文件（仅用于检查点/清单等小文件）。
bool readAll(const std::string& path, std::string* out, std::string* error);

// 原子写入：先写同目录临时文件再 rename，保证阶段完成后检查点完整可见。
bool atomicWrite(const std::string& path, const std::string& content,
                 std::string* error);

bool exists(const std::string& path);
bool removeFile(const std::string& path, std::string* error);

// FNV-1a 流式摘要：分块读取，避免把大文件整体载入内存。
uint64_t fnvFile(const std::string& path, size_t chunkBytes = 1 << 20);

}  // namespace media::fsutil

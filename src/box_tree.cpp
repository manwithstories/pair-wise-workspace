#include "box_tree.h"

#include "byte_reader.h"
#include "error.h"

#include <algorithm>
#include <cstring>
#include <set>
#include <sstream>

namespace mp4 {

namespace {

const std::set<std::string>& container_types() {
    static const std::set<std::string> types = {
        "moov", "trak", "mdia", "minf", "stbl", "edts", "dinf",
        "mvex", "moof", "traf", "mfra", "udta", "ilst", "tref",
    };
    return types;
}

bool is_container(const std::string& type) {
    return container_types().count(type) != 0;
}

void parse_boxes(ByteReader reader, uint64_t end_offset, std::vector<BoxNode>& out,
                 int depth) {
    if (depth > 64) {
        fail(ExitCode::FormatError, "box nesting exceeds 64 levels");
    }
    const uint64_t start_base = reader.absolute_position();
    while (reader.remaining() > 0) {
        if (reader.absolute_position() >= end_offset) {
            break;
        }
        if (end_offset - reader.absolute_position() < 8) {
            std::ostringstream oss;
            oss << "trailing truncated box at byte offset " << reader.absolute_position();
            fail(ExitCode::FormatError, oss.str());
        }
        const size_t box_start_local = reader.position();
        const uint64_t box_start = reader.absolute_position();
        const uint32_t size32 = reader.read_u32();
        const std::string_view type_view = reader.read_bytes(4);
        BoxNode node;
        node.type.assign(type_view.data(), type_view.size());
        node.offset = box_start;

        if (size32 == 0) {
            std::ostringstream oss;
            oss << "box '" << node.type << "' at byte offset " << box_start
                << " uses unsupported size 0";
            fail(ExitCode::FormatError, oss.str());
        }
        if (size32 == 1) {
            const uint64_t large_size = reader.read_u64();
            if (large_size > end_offset - box_start) {
                std::ostringstream oss;
                oss << "invalid 64-bit box size " << large_size << " for '"
                    << node.type << "' at byte offset " << box_start;
                fail(ExitCode::FormatError, oss.str());
            }
            node.size = large_size;
        } else {
            node.size = size32;
        }
        if (type_view == "uuid") {
            node.has_uuid = true;
            const std::string_view uuid = reader.read_bytes(16);
            std::memcpy(node.uuid.data(), uuid.data(), 16);
        }
        node.header_size = static_cast<uint64_t>(reader.absolute_position() - box_start);
        if (node.size < node.header_size) {
            std::ostringstream oss;
            oss << "box '" << node.type << "' at byte offset " << box_start
                << " has size " << node.size << " smaller than header";
            fail(ExitCode::FormatError, oss.str());
        }
        if (node.size > end_offset - box_start) {
            std::ostringstream oss;
            oss << "box '" << node.type << "' at byte offset " << box_start
                << " ends past its parent: size " << node.size;
            fail(ExitCode::FormatError, oss.str());
        }
        node.payload_offset = box_start + node.header_size;
        node.payload_size = node.size - node.header_size;
        const uint64_t box_end = box_start + node.size;

        if (is_container(node.type)) {
            ByteReader child_reader(reader.cursor(), static_cast<size_t>(node.payload_size),
                                    node.payload_offset);
            parse_boxes(child_reader, box_end, node.children, depth + 1);
        }
        out.push_back(std::move(node));

        const size_t consumed = static_cast<size_t>(box_end - box_start);
        reader.seek(box_start_local + consumed);
        if (reader.absolute_position() == end_offset) {
            break;
        }
    }
    if (reader.absolute_position() != end_offset) {
        std::ostringstream oss;
        oss << "box payload ended at byte offset " << reader.absolute_position()
            << ", expected " << end_offset;
        fail(ExitCode::FormatError, oss.str());
    }
    (void)start_base;
}

}  // namespace

const BoxNode* BoxNode::find(const std::string& child_type) const {
    for (const auto& child : children) {
        if (child.type == child_type) {
            return &child;
        }
    }
    return nullptr;
}

std::vector<const BoxNode*> BoxNode::find_all(const std::string& child_type) const {
    std::vector<const BoxNode*> result;
    for (const auto& child : children) {
        if (child.type == child_type) {
            result.push_back(&child);
        }
    }
    return result;
}

BoxTree::BoxTree(const std::vector<uint8_t>& data) {
    if (data.empty()) {
        fail(ExitCode::FormatError, "empty input at byte offset 0");
    }
    ByteReader reader(data.data(), data.size(), 0);
    parse_boxes(reader, data.size(), roots_, 0);
}

const BoxNode* BoxTree::find_root(const std::string& type) const {
    for (const auto& root : roots_) {
        if (root.type == type) {
            return &root;
        }
    }
    return nullptr;
}

const BoxNode* find_path(const BoxNode& root, const std::vector<std::string>& path) {
    const BoxNode* node = &root;
    for (const auto& type : path) {
        node = node->find(type);
        if (node == nullptr) {
            return nullptr;
        }
    }
    return node;
}

}  // namespace mp4

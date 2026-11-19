#include <cinttypes>
#include <cstdio>
#include <fstream>
#include <string>
#include <vector>

#include "cli_options.hpp"
#include "commands.hpp"
#include "damage_builder.hpp"
#include "media_types.hpp"
#include "packet_reader.hpp"
#include "pipeline.hpp"

namespace mfs {

int cmd_damage(const OptionTable& options) {
  std::string input;
  std::string kind;
  std::string out_path;
  if (!options.get("input", input) || !options.get("kind", kind) ||
      !options.get("out", out_path)) {
    std::fprintf(stderr, "damage requires --input --kind --out\n");
    return kExitUsageOrFormat;
  }
  static constexpr const char* kKinds[] = {"checksum", "length", "header",
                                           "tail",     "empty",  "magic",
                                           "required"};
  static constexpr DamageKind kDamage[] = {
      DamageKind::ChecksumMismatch, DamageKind::LengthOverflow,
      DamageKind::TruncatedHeader,  DamageKind::TruncatedTail,
      DamageKind::EmptySequence,    DamageKind::BadMagic,
      DamageKind::UnknownRequired};
  size_t index = 0;
  while (index < std::size(kKinds) && kind != kKinds[index]) {
    ++index;
  }
  if (index == std::size(kKinds)) {
    std::fprintf(stderr, "unknown damage kind: %s\n", kind.c_str());
    return kExitUsageOrFormat;
  }
  std::vector<uint8_t> source;
  try {
    source = read_file(input);
  } catch (const MediaError& error) {
    std::fprintf(stderr, "%s\n", error.what());
    return kExitUsageOrFormat;
  }
  DamageResult damaged;
  try {
    damaged = apply_damage(source, kDamage[index]);
  } catch (const MediaError& error) {
    std::fprintf(stderr, "damage failed: %s\n", error.what());
    return kExitUsageOrFormat;
  }
  std::ofstream out(out_path, std::ios::binary | std::ios::trunc);
  if (!out) {
    std::fprintf(stderr, "cannot write %s\n", out_path.c_str());
    return kExitInternal;
  }
  out.write(reinterpret_cast<const char*>(damaged.bytes.data()),
            static_cast<std::streamsize>(damaged.bytes.size()));
  if (!out) {
    return kExitInternal;
  }
  std::printf("{\n");
  std::printf("  \"kind\": \"%s\",\n", kind.c_str());
  std::printf("  \"out\": \"%s\",\n", out_path.c_str());
  std::printf("  \"offset\": %" PRIu64 "\n", damaged.offset);
  std::printf("}\n");
  return kExitOk;
}

int cmd_inspect(const OptionTable& options) {
  std::string input;
  if (!options.get("input", input)) {
    return kExitUsageOrFormat;
  }
  std::vector<uint8_t> data;
  try {
    data = read_file(input);
  } catch (const MediaError& error) {
    std::fprintf(stderr, "%s\n", error.what());
    return kExitUsageOrFormat;
  }
  PacketReader reader(data);
  uint64_t count = 0;
  uint64_t offset = 0;
  Packet packet;
  while (reader.next(packet)) {
    ++count;
    offset = packet.raw_offset;
  }
  std::printf("records=%" PRIu64 " last_offset=%" PRIu64 " errors=%zu\n", count,
              offset, reader.errors().size());
  for (const RepairError& error : reader.errors()) {
    std::printf("  %s offset=%" PRIu64 " detail=%s\n", to_string(error.type),
                error.offset, error.detail.c_str());
  }
  return kExitOk;
}

}  // namespace mfs

#include <cstdio>
#include <cstring>
#include <fstream>
#include <sstream>

extern "C" {
#include <libavcodec/avcodec.h>
}

#include "media_archive/packet_reader.h"

namespace media_archive {

const char* packet_flag_name(PacketFlag flag) {
  switch (flag) {
    case PacketFlag::Ok: return "OK";
    case PacketFlag::Duplicate: return "DUP";
    case PacketFlag::Reordered: return "REORDER";
    case PacketFlag::Gap: return "GAP";
  }
  return "UNKNOWN";
}

PacketFlag packet_flag_from_name(const std::string& name) {
  if (name == "DUP" || name == "DUPLICATE") return PacketFlag::Duplicate;
  if (name == "REORDER") return PacketFlag::Reordered;
  if (name == "GAP") return PacketFlag::Gap;
  return PacketFlag::Ok;
}

Diagnostic read_file_bounded(const std::string& path,
                             const ReaderLimits& limits,
                             std::vector<uint8_t>* out) {
  std::ifstream f(path, std::ios::binary | std::ios::ate);
  if (!f) return diag(Status::UsageError, "cannot open input file: " + path);
  const std::streamsize size = f.tellg();
  if (size < 0)
    return diag(Status::UsageError, "cannot stat input file: " + path);
  if (static_cast<uint64_t>(size) > limits.max_input_bytes) {
    Diagnostic d = diag(Status::UsageError,
                        "input exceeds size limit",
                        static_cast<int64_t>(limits.max_input_bytes));
    return d;
  }
  f.seekg(0, std::ios::beg);
  out->resize(static_cast<size_t>(size));
  if (size > 0 && !f.read(reinterpret_cast<char*>(out->data()), size))
    return diag(Status::IoError, "short read from: " + path);
  return {};
}

namespace {

AVCodecID ffmpeg_codec_id(CodecType codec) {
  switch (codec) {
    case CodecType::H264: return AV_CODEC_ID_H264;
    case CodecType::H265: return AV_CODEC_ID_HEVC;
    case CodecType::AV1:  return AV_CODEC_ID_AV1;
    case CodecType::Unknown: return AV_CODEC_ID_NONE;
  }
  return AV_CODEC_ID_NONE;
}

}  // namespace

Diagnostic parse_elementary_stream(const std::vector<uint8_t>& bytes,
                                   CodecType codec,
                                   const ReaderLimits& limits,
                                   std::vector<ParsedFrame>* frames) {
  if (codec == CodecType::Unknown)
    return diag(Status::UsageError,
                "explicit codec required (h264, h265 or av1)");
  const AVCodec* avcodec = avcodec_find_decoder(ffmpeg_codec_id(codec));
  if (!avcodec)
    return diag(Status::UsageError, "ffmpeg decoder unavailable for codec");
  AVCodecParserContext* parser = av_parser_init(avcodec->id);
  if (!parser)
    return diag(Status::MediaCorrupt, "cannot initialize codec parser");
  // The parser needs a live codec context even when we never decode frames.
  AVCodecContext* codec_ctx = avcodec_alloc_context3(avcodec);
  if (!codec_ctx) {
    av_parser_close(parser);
    return diag(Status::MediaCorrupt, "cannot allocate codec context");
  }

  frames->clear();
  const uint8_t* data = bytes.data();
  int remaining = static_cast<int>(bytes.size());
  int64_t consumed_offset = 0;
  AVPacket* pkt = av_packet_alloc();
  Diagnostic failure;

  while (remaining > 0) {
    uint8_t* out_data = nullptr;
    int out_size = 0;
    int used = av_parser_parse2(parser, codec_ctx, &out_data, &out_size, data,
                                remaining, AV_NOPTS_VALUE, AV_NOPTS_VALUE, 0);
    if (used < 0) {
      failure = diag(Status::MediaCorrupt,
                     "unrecognized stream data at byte offset",
                     consumed_offset);
      break;
    }
    if (out_size > 0) {
      if (static_cast<uint32_t>(out_size) > limits.max_block_bytes) {
        failure = diag(Status::UsageError,
                       "single frame exceeds block resource limit",
                       consumed_offset);
        break;
      }
      ParsedFrame frame;
      frame.source_offset = static_cast<uint64_t>(consumed_offset);
      frame.size_bytes = static_cast<uint32_t>(out_size);
      frame.frame_type = static_cast<uint32_t>(parser->pict_type);
      frame.keyframe = (parser->key_frame == 1);
      if (frame.source_offset > limits.max_index_offset) {
        failure = diag(Status::UsageError,
                       "index offset exceeds configured limit",
                       static_cast<int64_t>(frame.source_offset));
        break;
      }
      frames->push_back(frame);
      consumed_offset += used;
    } else {
      consumed_offset += used;
    }
    data += used;
    remaining -= used;
  }

  av_packet_free(&pkt);
  av_parser_close(parser);
  avcodec_free_context(&codec_ctx);
  if (!failure.ok()) return failure;
  if (frames->empty()) {
    return diag(Status::MediaCorrupt,
                "no parseable frames; not a valid " +
                    std::string(codec_name(codec)) + " elementary stream",
                0);
  }
  return {};
}

Diagnostic parse_packet_manifest(const std::string& path,
                                 std::vector<TransportPacket>* packets) {
  std::ifstream f(path);
  if (!f) return diag(Status::UsageError, "cannot open packet manifest: " + path);
  packets->clear();
  std::string line;
  bool header = true;
  size_t line_no = 0;
  while (std::getline(f, line)) {
    ++line_no;
    if (line.empty()) continue;
    if (header) {  // skip the CSV header row
      header = false;
      continue;
    }
    std::stringstream ss(line);
    std::string seq, pts, off, size, flag, send, recv;
    if (!std::getline(ss, seq, ',') || !std::getline(ss, pts, ',') ||
        !std::getline(ss, off, ',') || !std::getline(ss, size, ','))
      return diag(Status::MediaCorrupt,
                  "malformed manifest row", static_cast<int64_t>(line_no));
    std::getline(ss, flag, ',');
    std::getline(ss, send, ',');
    std::getline(ss, recv, ',');
    TransportPacket p;
    try {
      p.seq = static_cast<uint32_t>(std::stoul(seq));
      p.pts_ticks = std::stoull(pts);
      p.source_offset = std::stoll(off);
      p.size_bytes = static_cast<uint32_t>(std::stoul(size));
    } catch (const std::exception&) {
      return diag(Status::MediaCorrupt,
                  "non-numeric manifest field",
                  static_cast<int64_t>(line_no));
    }
    p.flag = packet_flag_from_name(flag);
    if (!send.empty()) p.send_ts_ms = std::stod(send);
    if (!recv.empty()) p.recv_ts_ms = std::stod(recv);
    packets->push_back(p);
  }
  if (packets->empty())
    return diag(Status::MediaCorrupt, "packet manifest is empty");
  return {};
}

Diagnostic write_packet_manifest(const std::string& path,
                                 const std::vector<TransportPacket>& packets) {
  std::ofstream f(path, std::ios::binary | std::ios::trunc);
  if (!f) return diag(Status::IoError, "cannot write manifest: " + path);
  f << "seq,pts,offset,size,flag,send_ms,recv_ms\n";
  for (const auto& p : packets) {
    f << p.seq << ',' << p.pts_ticks << ',' << p.source_offset << ','
      << p.size_bytes << ',' << packet_flag_name(p.flag) << ','
      << p.send_ts_ms << ',' << p.recv_ts_ms << '\n';
  }
  if (!f) return diag(Status::IoError, "manifest write failed: " + path);
  return {};
}

}  // namespace media_archive

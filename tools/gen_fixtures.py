#!/usr/bin/env python3
"""Controlled H.264 Annex B fixture generator for h264probe.

Pure stdlib. Streams are synthetic but structurally valid: SPS/PPS/slice
headers follow the bit syntax the h264probe parsers expect, so generated
files double as regression fixtures with known ground truth.
"""

import argparse
import os
import random
import sys


class BitWriter:
    def __init__(self):
        self._buf = bytearray()
        self._nbits = 0

    def put(self, value, nbits):
        for i in range(nbits - 1, -1, -1):
            bit = (value >> i) & 1
            if self._nbits % 8 == 0:
                self._buf.append(0)
            if bit:
                self._buf[-1] |= 0x80 >> (self._nbits % 8)
            self._nbits += 1

    def put_ue(self, value):
        code = value + 1
        nbits = code.bit_length()
        self.put(0, nbits - 1)
        self.put(code, nbits)

    def put_se(self, value):
        self.put_ue(2 * value - 1 if value > 0 else -2 * value)

    def align_zero(self):
        while self._nbits % 8:
            self.put(0, 1)

    def rbsp(self):
        self.put(1, 1)  # rbsp_stop_one_bit
        self.align_zero()
        return bytes(self._buf)

    def raw_bytes(self):
        self.align_zero()
        return bytes(self._buf)


def escape_ebsp(rbsp):
    out = bytearray()
    zeros = 0
    for b in rbsp:
        if zeros >= 2 and b <= 0x03:
            out.append(0x03)
            zeros = 0
        out.append(b)
        zeros = zeros + 1 if b == 0 else 0
    return bytes(out)


def nal(nal_type, ref_idc, rbsp, four_byte=False):
    start_code = b"\x00\x00\x00\x01" if four_byte else b"\x00\x00\x01"
    return start_code + bytes([(ref_idc << 5) | nal_type]) + escape_ebsp(rbsp)


def build_sps(sps_id=0, profile=100, level=40, width_mbs=22, height_map_units=18,
              frame_mbs_only=True, mbaff=False, log2_frame_num=8, log2_poc_lsb=8,
              fps=25):
    w = BitWriter()
    w.put(profile, 8)
    w.put(0, 8)  # constraint flags + reserved
    w.put(level, 8)
    w.put_ue(sps_id)
    if profile in (100, 110, 122, 244, 44, 83, 86, 118, 128):
        w.put_ue(1)   # chroma_format_idc = 4:2:0
        w.put_ue(0)   # bit_depth_luma_minus8
        w.put_ue(0)   # bit_depth_chroma_minus8
        w.put(0, 1)   # qpprime_y_zero_transform_bypass_flag
        w.put(0, 1)   # seq_scaling_matrix_present_flag
    w.put_ue(log2_frame_num - 4)
    w.put_ue(0)  # pic_order_cnt_type = 0
    w.put_ue(log2_poc_lsb - 4)
    w.put_ue(4)   # max_num_ref_frames
    w.put(0, 1)   # gaps_in_frame_num_value_allowed_flag
    w.put_ue(width_mbs - 1)
    w.put_ue(height_map_units - 1)
    w.put(1 if frame_mbs_only else 0, 1)
    if not frame_mbs_only:
        w.put(1 if mbaff else 0, 1)
    w.put(1, 1)  # direct_8x8_inference_flag
    w.put(0, 1)  # frame_cropping_flag
    w.put(1, 1)  # vui_parameters_present_flag
    w.put(0, 1)  # aspect_ratio_info_present_flag
    w.put(0, 1)  # overscan_info_present_flag
    w.put(0, 1)  # video_signal_type_present_flag
    w.put(0, 1)  # chroma_loc_info_present_flag
    w.put(1, 1)            # timing_info_present_flag
    w.put(1, 32)           # num_units_in_tick
    w.put(2 * fps, 32)     # time_scale -> fps = time_scale / (2 * num_units)
    w.put(1, 1)            # fixed_frame_rate_flag
    w.put(0, 1)  # nal_hrd_parameters_present_flag
    w.put(0, 1)  # vcl_hrd_parameters_present_flag
    w.put(0, 1)  # pic_struct_present_flag
    w.put(0, 1)  # bitstream_restriction_flag
    return w.rbsp()


def build_pps(pps_id=0, sps_id=0):
    w = BitWriter()
    w.put_ue(pps_id)
    w.put_ue(sps_id)
    w.put(0, 1)   # entropy_coding_mode_flag (CAVLC)
    w.put(0, 1)   # bottom_field_pic_order_in_frame_present_flag
    w.put_ue(0)   # num_slice_groups_minus1
    w.put_ue(0)   # num_ref_idx_l0_default_active_minus1
    w.put_ue(0)   # num_ref_idx_l1_default_active_minus1
    w.put(0, 1)   # weighted_pred_flag
    w.put(0, 2)   # weighted_bipred_idc
    w.put_se(0)   # pic_init_qp_minus26
    w.put_se(0)   # pic_init_qs_minus26
    w.put_se(0)   # chroma_qp_index_offset
    w.put(0, 1)   # deblocking_filter_control_present_flag
    w.put(0, 1)   # constrained_intra_pred_flag
    w.put(0, 1)   # redundant_pic_cnt_present_flag
    return w.rbsp()


def build_sei(recovery_frame_cnt=None, buffering_sps_id=None):
    payload = bytearray()
    if buffering_sps_id is not None:
        w = BitWriter()
        w.put_ue(buffering_sps_id)
        body = w.rbsp()
        payload += b"\x00" + bytes([len(body)]) + body
    if recovery_frame_cnt is not None:
        w = BitWriter()
        w.put_ue(recovery_frame_cnt)
        w.put(1, 1)  # exact_match_flag
        w.put(0, 1)  # broken_link_flag
        w.put(0, 2)  # changing_slice_group_idc
        body = w.rbsp()
        payload += b"\x01" + bytes([len(body)]) + body
    payload += b"\x80"  # rbsp_trailing_bits
    return bytes(payload)


def build_slice_header(first_mb, slice_type, pps_id, frame_num, log2_frame_num,
                       idr, idr_pic_id, poc_lsb, log2_poc_lsb, field, bottom,
                       frame_mbs_only):
    w = BitWriter()
    w.put_ue(first_mb)
    w.put_ue(slice_type)
    w.put_ue(pps_id)
    w.put(frame_num, log2_frame_num)
    if not frame_mbs_only:
        w.put(1 if field else 0, 1)
        if field:
            w.put(1 if bottom else 0, 1)
    if idr:
        w.put_ue(idr_pic_id)
    w.put(poc_lsb, log2_poc_lsb)
    if idr:
        w.put(0, 1)  # no_output_of_prior_pics_flag
        w.put(0, 1)  # long_term_reference_flag
    else:
        w.put(0, 1)  # adaptive_ref_pic_marking_mode_flag
    w.put_se(0)      # slice_qp_delta
    return w.raw_bytes()


LOG2_FRAME_NUM = 8
LOG2_POC_LSB = 8
WIDTH_MBS = 22          # 352 px
HEIGHT_MAP_PROGRESSIVE = 18  # 288 px
HEIGHT_MAP_FIELD = 9


def write_slice(out, nal_type, ref_idc, header, payload_len, rng, chunky):
    out.write(b"\x00\x00\x01")
    out.write(bytes([(ref_idc << 5) | nal_type]))
    out.write(escape_ebsp(header))
    if payload_len > 0:
        if chunky:
            block = b"\xff" * (1 << 20)
            remaining = payload_len
            while remaining > 0:
                take = min(remaining, len(block))
                out.write(block[:take])
                remaining -= take
        else:
            out.write(bytes(rng.randrange(1, 256) for _ in range(payload_len)))
    out.write(b"\x80")


def build_stream(path, seed, gops, slices_per_pic, interlace, frames_per_gop=8,
                 target_bytes=None, with_b=False, recovery_every=0):
    rng = random.Random(seed)
    frame_mbs_only = interlace == "progressive"
    mbaff = interlace == "mbaff"
    height_map = HEIGHT_MAP_PROGRESSIVE if frame_mbs_only else HEIGHT_MAP_FIELD
    pic_mbs = WIDTH_MBS * height_map
    fields_per_frame = 2 if interlace == "paff" else 1
    sps_rbsp = build_sps(frame_mbs_only=frame_mbs_only, mbaff=mbaff,
                         height_map_units=height_map,
                         log2_frame_num=LOG2_FRAME_NUM,
                         log2_poc_lsb=LOG2_POC_LSB)
    pps_rbsp = build_pps()

    slices_total = gops * frames_per_gop * fields_per_frame * slices_per_pic
    if target_bytes:
        payload_len = max(64, target_bytes // max(1, slices_total))
        chunky = True
    else:
        payload_len = 0  # per-slice randomized below
        chunky = False

    frame_num = 0
    idr_pic_id = 0
    poc = 0
    with open(path, "wb") as out:
        for g in range(gops):
            use_recovery = recovery_every > 0 and g % recovery_every == 0 and g > 0
            out.write(nal(7, 3, sps_rbsp, four_byte=True))
            out.write(nal(8, 3, pps_rbsp, four_byte=True))
            if use_recovery:
                out.write(nal(6, 0, build_sei(recovery_frame_cnt=0,
                                              buffering_sps_id=0)))
            for f in range(frames_per_gop):
                is_anchor = f == 0
                idr = is_anchor and not use_recovery
                if is_anchor:
                    slice_type = 7  # I
                elif with_b and f % 3 == 2:
                    slice_type = 1  # B
                else:
                    slice_type = 0  # P
                if idr:
                    poc = 0  # IDR resets picture order count
                for field_idx in range(fields_per_frame):
                    field = fields_per_frame == 2
                    bottom = field_idx == 1
                    poc_lsb = (poc + field_idx) % (1 << LOG2_POC_LSB)
                    for s in range(slices_per_pic):
                        first_mb = s * pic_mbs // slices_per_pic
                        header = build_slice_header(
                            first_mb, slice_type, 0, frame_num, LOG2_FRAME_NUM,
                            idr, idr_pic_id, poc_lsb, LOG2_POC_LSB,
                            field, bottom, frame_mbs_only)
                        plen = payload_len if chunky else rng.randrange(24, 96)
                        write_slice(out, 5 if idr else 1, 3, header, plen, rng, chunky)
                if idr:
                    idr_pic_id = (idr_pic_id + 1) % 65536
                frame_num = (frame_num + 1) % (1 << LOG2_FRAME_NUM)
                poc = (poc + 2) % (1 << LOG2_POC_LSB)
    return slices_total


def main():
    ap = argparse.ArgumentParser(description="H.264 Annex B fixture generator")
    ap.add_argument("--out", required=True, help="output directory")
    ap.add_argument("--seed", type=int, default=7)
    ap.add_argument("--gops", type=int, default=24)
    ap.add_argument("--slices", default="1",
                    help="comma list of slices-per-picture, one file per value")
    ap.add_argument("--interlace", choices=["progressive", "paff", "mbaff"],
                    default="progressive")
    ap.add_argument("--frames-per-gop", type=int, default=8)
    ap.add_argument("--with-b", action="store_true")
    ap.add_argument("--recovery-every", type=int, default=0,
                    help="every K-th GOP (after the first) opens with a "
                         "recovery point SEI instead of IDR")
    ap.add_argument("--large-mb", type=int, default=0,
                    help="emit a single large.h264 of roughly N MiB")
    args = ap.parse_args()

    os.makedirs(args.out, exist_ok=True)
    if args.large_mb:
        path = os.path.join(args.out, "large.h264")
        n = build_stream(path, args.seed, gops=50, slices_per_pic=1,
                         interlace="progressive", frames_per_gop=4,
                         target_bytes=args.large_mb << 20)
        size = os.path.getsize(path)
        print(f"{path}: {size} bytes, {n} slices")
        return 0

    for spec in args.slices.split(","):
        n_slices = int(spec)
        name = f"{args.interlace}_{n_slices}slice.h264"
        path = os.path.join(args.out, name)
        n = build_stream(path, args.seed, args.gops, n_slices, args.interlace,
                         frames_per_gop=args.frames_per_gop, with_b=args.with_b,
                         recovery_every=args.recovery_every)
        size = os.path.getsize(path)
        print(f"{path}: {size} bytes, {n} slices")
    return 0


if __name__ == "__main__":
    sys.exit(main())

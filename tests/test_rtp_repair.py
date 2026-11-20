"""RTP/RTCP parsing, counter wraparound and the shared repair model."""

from __future__ import annotations

import struct

import pytest

from archiveflow.errors import UnrecoverableError
from archiveflow.repair import (BURST_MIN_LEN, Emitted, FrameReassembler,
                                RepairStats, ReorderBuffer)
from archiveflow.rtp import (CODEC_AUDIO, CODEC_VIDEO, FLAG_KEYFRAME,
                             PacketOrganizer, RtcpSR, pack_payload,
                             parse_rtcp_sr, parse_rtp, unpack_payload)


def rtp(seq: int, ts: int, ssrc: int = 1, pt: int = 96,
        marker: bool = False, payload: bytes = b"x") -> bytes:
    return struct.pack(">BBHII", 0x80, (0x80 if marker else 0) | pt,
                       seq & 0xFFFF, ts & 0xFFFFFFFF, ssrc) + payload


class TestRtpParsing:
    def test_roundtrip_basic_fields(self):
        pkt = parse_rtp(rtp(0x1234, 0xAAAA, ssrc=0x42, marker=True), 7)
        assert pkt.seq == 0x1234
        assert pkt.timestamp == 0xAAAA
        assert pkt.ssrc == 0x42
        assert pkt.marker is True
        assert pkt.arrival == 7
        assert pkt.payload == b"x"

    def test_rejects_bad_version_and_short_packet(self):
        with pytest.raises(UnrecoverableError):
            parse_rtp(b"\x00" * 12)  # version 0
        with pytest.raises(UnrecoverableError):
            parse_rtp(b"\x80\x60" + b"\x00" * 4)  # too short

    def test_csrc_extension_and_padding(self):
        # V=2, P=1, X=1, CC=1: 1 CSRC then a header extension then padding.
        body = struct.pack(">BBHII", 0xB1, 0xA0, 1, 10, 1)
        body += struct.pack(">I", 9)  # one CSRC
        body += struct.pack(">HH", 0xBEDE, 0)  # extension, length 0 words
        body += b"DATA" + b"\x01"  # one padding byte
        pkt = parse_rtp(body)
        assert pkt.payload == b"DATA"

    def test_payload_header_roundtrip(self):
        blob = pack_payload(5, 1, 3, True, CODEC_VIDEO, b"chunk")
        hdr, chunk = unpack_payload(blob)
        assert (hdr.frame_index, hdr.part, hdr.parts) == (5, 1, 3)
        assert hdr.keyframe is True and hdr.codec == CODEC_VIDEO
        assert chunk == b"chunk"

    def test_payload_rejects_bad_magic_and_parts(self):
        with pytest.raises(UnrecoverableError):
            unpack_payload(b"\x00" * 9)
        bad = struct.pack(">2sIBBBB", b"XX", 1, 0, 1, 0, CODEC_AUDIO)
        with pytest.raises(UnrecoverableError):
            unpack_payload(bad)


class TestWraparound:
    def test_sequence_wraparound_extends_to_64bit(self):
        org = PacketOrganizer()
        first = org.push(parse_rtp(rtp(0xFFFE, 1000)))
        second = org.push(parse_rtp(rtp(0x0001, 1060)))
        assert first.seq == 0xFFFE
        assert second.seq == 0x10001  # continued into the next cycle

    def test_timestamp_wraparound(self):
        org = PacketOrganizer()
        org.push(parse_rtp(rtp(1, 0xFFFFFFF0)))
        nxt = org.push(parse_rtp(rtp(2, 0x00000010)))
        assert nxt.timestamp == 0x100000010

    def test_streams_grouped_by_ssrc(self):
        org = PacketOrganizer()
        org.push(parse_rtp(rtp(0, 0, ssrc=1)))
        org.push(parse_rtp(rtp(0, 0, ssrc=2)))
        streams = org.streams()
        assert set(streams) == {1, 2}
        assert all(len(v) == 1 for v in streams.values())


class TestReorderBuffer:
    def _feed(self, window, seq_values):
        stats = RepairStats()
        buf = ReorderBuffer(window, stats)
        emitted = []
        for s in seq_values:
            emitted.extend(buf.push(parse_rtp(rtp(s, s * 10))))
        emitted.extend(buf.flush())
        return emitted, stats

    def test_in_order_passes_through(self):
        emitted, stats = self._feed(4, range(5))
        assert [e.seq for e in emitted] == [0, 1, 2, 3, 4]
        assert all(e.packet is not None for e in emitted)
        assert stats.lost_packets == 0

    def test_reordering_within_window_is_absorbed(self):
        # 0,2,1,3 -> committed in order, no losses once the window slides.
        emitted, stats = self._feed(8, [0, 2, 1, 3, 4, 5, 6, 7, 8])
        seqs = [e.seq for e in emitted]
        assert seqs == sorted(seqs)
        assert stats.lost_packets == 0
        assert stats.late_packets == 0

    def test_duplicates_counted_and_dropped(self):
        stats = RepairStats()
        buf = ReorderBuffer(8, stats)
        pkts = []
        for s in [0, 1, 1, 2]:
            pkts.extend(buf.push(parse_rtp(rtp(s, s))))
        pkts.extend(buf.flush())
        assert stats.duplicates == 1
        assert [e.seq for e in pkts] == [0, 1, 2]

    def test_gap_marked_as_loss_placeholder(self):
        emitted, stats = self._feed(4, [0, 1, 5, 6, 7, 8])
        lost_slots = [e.seq for e in emitted if e.packet is None]
        assert lost_slots == [2, 3, 4]
        assert stats.lost_packets == 3
        # A 3-packet gap is below the burst threshold.
        assert stats.loss_bursts == 0

    def test_burst_gap_flagged_as_burst(self):
        emitted, stats = self._feed(4, [0, BURST_MIN_LEN + 1, BURST_MIN_LEN + 2,
                                        BURST_MIN_LEN + 3, BURST_MIN_LEN + 4,
                                        BURST_MIN_LEN + 5])
        assert stats.loss_bursts == 1
        assert stats.lost_packets == BURST_MIN_LEN

    def test_late_packet_after_commit_never_rewrites(self):
        stats = RepairStats()
        buf = ReorderBuffer(4, stats)
        out = []
        # Anchor the stream at 0, then jump ahead so 1..9 commit as losses.
        out.extend(buf.push(parse_rtp(rtp(0, 0))))
        for s in [10, 11, 12, 13, 14]:
            out.extend(buf.push(parse_rtp(rtp(s, s))))
        # seq 1..9 are now committed loss placeholders; seq 5 arrives late.
        late = buf.push(parse_rtp(rtp(5, 50)))
        out.extend(buf.flush())
        assert late == []
        assert stats.late_packets == 1
        # The committed loss placeholder for seq 5 stays in place.
        slot5 = next(e for e in out if e.seq == 5)
        assert slot5.packet is None


class TestFrameReassembler:
    def test_complete_frame_full_confidence(self):
        reasm = FrameReassembler()
        frame = None
        for part in range(3):
            payload = pack_payload(0, part, 3, part == 0, CODEC_VIDEO,
                                   bytes([part]))
            pkt = parse_rtp(rtp(part, 100, payload=payload))
            frame = reasm.push(Emitted(seq=part, packet=pkt))
        assert frame is not None
        assert frame.confidence == 1.0
        assert frame.data == b"\x00\x01\x02"

    def test_partial_frame_at_finish_has_reduced_confidence(self):
        reasm = FrameReassembler()
        payload = pack_payload(7, 0, 4, False, CODEC_VIDEO, b"a")
        pkt = parse_rtp(rtp(0, 50, payload=payload))
        assert reasm.push(Emitted(seq=0, packet=pkt)) is None
        leftover = reasm.finish()
        assert len(leftover) == 1
        assert leftover[0].index == 7
        assert leftover[0].confidence == pytest.approx(0.25)

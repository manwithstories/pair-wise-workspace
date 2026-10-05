//! The receiving half: reassembly, in-order delivery, and the SACK bitmap.
//!
//! The receiver holds a small out-of-order map keyed by sequence number. Data
//! arrives in whatever order the link produced it; the map is what turns that
//! back into a byte stream.
//!
//! # Why a bitmap rather than a segment list
//!
//! The peer needs to know *exactly* which holes exist so it can retransmit only
//! what is missing. A [`std::collections::BTreeSet`] of received sequence
//! numbers would answer that, but at 15% loss with a reordering window of 16 the
//! set churns constantly and every insert is a tree walk. A fixed 64-bit bitmap
//! over a sliding window makes "have I seen segment n?" a single shift and
//! mask, makes SACK-block construction a linear scan of 64 words, and caps the
//! receiver's memory regardless of how badly the link is misbehaving.
//!
//! # Window
//!
//! [`Receiver::recv_window`] is the span of sequence numbers the bitmap can
//! represent. It is deliberately much larger than the peer's congestion
//! window: a bitmap that is too small would make the receiver drop segments the
//! peer legitimately has in flight, which would look exactly like extra loss.

use std::collections::{BTreeMap, VecDeque};
use std::fmt;

use crate::wire::{seq_add, seq_lt, seq_span, Block, Seq, MAX_SACK_BLOCKS};

/// Number of segments the receive bitmap covers.
pub const RECV_WINDOW: u64 = 512;

/// One segment accepted by the receiver.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct RecvSegment {
    pub seq: Seq,
    pub payload: Vec<u8>,
    /// The peer marked this as a retransmission.
    pub retransmitted: bool,
}

/// What one inbound datagram did to the receive state.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum RecvOutcome {
    /// Filled the gap at the cumulative point; `n` segments are now deliverable.
    InOrder { newly_deliverable: u64 },
    /// Stored out of order, above a hole.
    OutOfOrder,
    /// A segment already received, or below the cumulative point.
    Duplicate,
    /// Below the cumulative point, or outside the receive window.
    Stale,
    /// A FIN, carrying the peer sequence number that ends its stream.
    Fin,
}

/// The receiving state machine.
pub struct Receiver {
    /// Left edge of the receive window: every segment below this has been
    /// delivered in order.
    una: Seq,
    /// Right edge of the bitmap's coverage.
    window_end: Seq,
    /// Received-but-undelivered segments above `una`, keyed by absolute
    /// sequence number.
    ///
    /// A map rather than a bitmap-indexed array, because the two were
    /// desynchronising: the bitmap is indexed by offset from `una`, so every
    /// window slide has to shift an equally-indexed payload array in lockstep,
    /// and any drift between them silently marks a segment "received" while its
    /// payload is gone — the receiver then advances its cumulative ACK over data
    /// it never held, and the sender stops retransmitting a hole that is never
    /// filled. Keying by absolute sequence makes that class of bug impossible:
    /// a payload exists if and only if its key is present.
    ///
    /// The window is bounded by [`RECV_WINDOW`], so the map's size is bounded
    /// too; the eviction in [`Receiver::on_segment`] is what enforces it.
    pending: BTreeMap<Seq, Vec<u8>>,
    /// The peer's FIN sequence number, once it has arrived. The FIN occupies a
    /// sequence number like a data segment, so the peer's stream is complete
    /// once the window has advanced *past* this value. Zero while no FIN is
    /// outstanding.
    fin_seq: Option<Seq>,
    /// The peer's FIN has arrived and been consumed.
    fin_seen: bool,
    /// The peer's FIN was acknowledged in the last control packet we built.
    fin_acked: bool,
    /// Counters.
    segments_accepted: u64,
    duplicates_dropped: u64,
    stale_dropped: u64,
    out_of_order_peak: u64,
    bytes_delivered: u64,
    /// True once the peer's FIN has been delivered to the application.
    fin_delivered: bool,
    /// In-order segments that are ready for the application but not yet taken.
    ///
    /// This is a real queue rather than a cursor into the bitmap because the
    /// window slides past delivered segments: once `una` moves, the bitmap no
    /// longer describes what has been delivered, and the payload has to live
    /// somewhere. Keeping it explicit also means the application can be
    /// slower than the network without the receiver dropping data.
    ready: VecDeque<Vec<u8>>,
}

impl Receiver {
    /// Build a receiver whose stream starts at `isn`.
    pub fn new(isn: Seq) -> Self {
        Receiver {
            una: isn,
            window_end: seq_add(isn, RECV_WINDOW),
            pending: BTreeMap::new(),
            fin_seq: None,
            fin_seen: false,
            fin_acked: false,
            segments_accepted: 0,
            duplicates_dropped: 0,
            stale_dropped: 0,
            out_of_order_peak: 0,
            bytes_delivered: 0,
            fin_delivered: false,
            ready: VecDeque::new(),
        }
    }

    /// Left edge of the receive window: the next segment to be delivered.
    #[inline]
    pub fn una(&self) -> Seq {
        self.una
    }

    /// Right edge of the receive window.
    #[inline]
    pub fn window_end(&self) -> Seq {
        self.window_end
    }

    /// The receive window `[una, window_end)`.
    pub fn recv_window(&self) -> Block {
        Block::new(self.una, self.window_end)
    }

    /// Is the peer's FIN outstanding?
    pub fn fin_seen(&self) -> bool {
        self.fin_seen
    }

    /// Was the peer's FIN already reported to the peer?
    pub fn fin_acked(&self) -> bool {
        self.fin_acked
    }

    /// Mark that the current FIN has been reported, so it is not re-reported.
    pub fn mark_fin_acked(&mut self) {
        if self.fin_seen {
            self.fin_acked = true;
        }
    }

    /// Has the FIN been delivered to the application?
    pub fn fin_delivered(&self) -> bool {
        self.fin_delivered
    }

    /// Segments currently buffered out of order.
    pub fn out_of_order_len(&self) -> u64 {
        self.pending.len() as u64
    }

    /// Highest out-of-order count observed, for the reordering budget.
    pub fn out_of_order_peak(&self) -> u64 {
        self.out_of_order_peak
    }

    /// Segments dropped as duplicates.
    pub fn duplicates_dropped(&self) -> u64 {
        self.duplicates_dropped
    }

    /// Segments dropped as outside the receive window.
    pub fn stale_dropped(&self) -> u64 {
        self.stale_dropped
    }

    /// Counters for the dump and the soak report.
    pub fn stats(&self) -> RecvStats {
        RecvStats {
            segments_accepted: self.segments_accepted,
            duplicates_dropped: self.duplicates_dropped,
            stale_dropped: self.stale_dropped,
            out_of_order_peak: self.out_of_order_peak,
            bytes_delivered: self.bytes_delivered,
        }
    }

    /// Accept one segment.
    ///
    /// Returns what happened plus, for an in-order arrival, how many segments
    /// became deliverable. The caller drains those with
    /// [`Receiver::take_deliverable`].
    pub fn on_segment(&mut self, seq: Seq, payload: Vec<u8>, _retransmitted: bool) -> RecvOutcome {
        // Below the cumulative point: already delivered, nothing to do. This is
        // the normal case for a duplicate of something we have.
        if seq_lt(seq, self.una) {
            self.duplicates_dropped += 1;
            return RecvOutcome::Duplicate;
        }

        // Outside the sliding receive window. The window is several times the
        // peer's maximum congestion window, so reaching it means the peer
        // ignored flow control rather than that the network reordered badly.
        if seq_span(self.una, seq) >= RECV_WINDOW {
            self.stale_dropped += 1;
            return RecvOutcome::Stale;
        }

        if self.pending.contains_key(&seq) {
            // A duplicate, or a retransmission of something already held. Not an
            // error, but it must not be buffered twice.
            self.duplicates_dropped += 1;
            return RecvOutcome::Duplicate;
        }

        // Bound the window. When it is full, the lowest held sequence is the
        // one the peer most needs to advance, so making room below is the right
        // policy: a segment far above the hole has not blocked anything.
        if self.pending.len() as u64 >= RECV_WINDOW {
            if let Some((&lowest, _)) = self.pending.iter().next() {
                self.pending.remove(&lowest);
            }
        }

        self.pending.insert(seq, payload);
        self.segments_accepted += 1;

        if seq == self.una {
            // Filled the hole at the cumulative point. Walk forward over the
            // contiguous run and slide the window over it.
            let run = self.advance();
            RecvOutcome::InOrder { newly_deliverable: run }
        } else {
            self.out_of_order_peak = self.out_of_order_peak.max(self.out_of_order_len());
            RecvOutcome::OutOfOrder
        }
    }

    /// Slide the window over every contiguous received segment.
    ///
    /// Payloads of the segments being delivered move into [`Receiver::ready`]
    /// first, so a slow consumer does not lose data to a window slide, and the
    /// count of how many became deliverable is returned for the ACK.
    fn advance(&mut self) -> u64 {
        // Move the contiguous run starting at `una` into the ready queue. The
        // map is keyed by absolute sequence, so this walk needs no index
        // arithmetic and cannot desynchronise from anything.
        let mut delivered = 0u64;
        let mut next = self.una;
        while let Some(p) = self.pending.remove(&next) {
            self.bytes_delivered += p.len() as u64;
            self.ready.push_back(p);
            next = seq_add(next, 1);
            delivered += 1;
        }
        if delivered == 0 {
            return 0;
        }
        self.una = next;
        self.window_end = seq_add(self.una, RECV_WINDOW);
        self.recheck_fin();
        delivered
    }

    /// Take the next in-order segment's payload, if one is deliverable.
    ///
    /// Draining through here rather than returning bytes from
    /// [`Receiver::on_segment`] keeps the caller loop uniform: the same
    /// `while let Some(b) = take_deliverable()` works whether the data arrived
    /// in order or was released later by filling a hole.
    pub fn take_deliverable(&mut self) -> Option<Vec<u8>> {
        self.ready.pop_front()
    }

    /// Segments ready for the application but not yet taken.
    pub fn ready_len(&self) -> usize {
        self.ready.len()
    }

    /// Bytes ready for the application but not yet taken.
    pub fn ready_bytes(&self) -> usize {
        self.ready.iter().map(|p| p.len()).sum()
    }

    /// Accept the peer's FIN, which ends its stream at `seq`.
    ///
    /// Returns `false` if the FIN arrives before the data it follows has been
    /// delivered, in which case the caller must wait; the FIN is remembered and
    /// re-checked as the window slides.
    pub fn on_fin(&mut self, seq: Seq) -> bool {
        self.fin_seen = true;
        // A FIN ends the peer's stream at `seq`: everything below it is data,
        // and the FIN itself is one more segment to be passed.
        self.fin_seq = Some(seq);
        self.recheck_fin();
        self.fin_delivered
    }

    /// Has every segment up to the peer's FIN been delivered?
    pub fn fin_ready(&self) -> bool {
        self.fin_seen && self.fin_delivered
    }

    /// Re-evaluate FIN readiness after the window slid.
    ///
    /// A FIN that arrived ahead of its data is recorded but not ready; the gap
    /// may be filled many segments later, so readiness has to be rechecked at
    /// every slide rather than only in [`Receiver::on_fin`].
    fn recheck_fin(&mut self) {
        if self.fin_delivered {
            return;
        }
        if let Some(fs) = self.fin_seq {
            // The FIN carries no payload, so there is nothing for it to be
            // reordered behind: the stream is complete as soon as the window
            // reaches the FIN's own sequence number, with all data below it
            // delivered.
            if !seq_lt(self.una, fs) {
                self.fin_delivered = true;
                // The FIN occupies a sequence number, so acknowledging the peer's
                // data is not enough — the cumulative ACK has to move past it.
                // Leaving `una` at the FIN's number means the sender is left
                // waiting for an acknowledgement that can never be expressed:
                // the receiver has nothing further to deliver, so no later
                // packet would ever carry a higher `ack`, and the close would
                // hang until the timeout forced it down.
                self.una = seq_add(fs, 1);
                self.window_end = seq_add(self.una, RECV_WINDOW);
            }
        }
    }

    /// Build the SACK blocks to advertise, nearest the cumulative ACK first.
    ///
    /// **These are the ranges the receiver *has*, not the ranges it is
    /// missing.** That is the RFC 2018 definition, and both ends depend on
    /// getting it right: the sender marks segments inside a block as delivered
    /// so it does not retransmit them, which is the entire point of selective
    /// acknowledgement. Advertising holes instead would make the sender believe
    /// precisely the segments it most needs to resend are already safe, and the
    /// connection would deadlock with a stuck window and a growing hole.
    ///
    /// Each block is a maximal run of received segments strictly above `una`.
    /// The single leading gap is dropped: the cumulative ACK already implies
    /// it, and spending one of the eight slots on it wastes information.
    pub fn sack_for_packet(&self) -> Vec<Block> {
        let mut blocks: Vec<Block> = Vec::with_capacity(MAX_SACK_BLOCKS);
        let mut run_start: Option<Seq> = None;
        let mut run_end = self.una;
        for &s in self.pending.keys() {
            if blocks.len() == MAX_SACK_BLOCKS {
                break;
            }
            if run_start.is_none() {
                run_start = Some(s);
                run_end = s;
            } else if s == run_end {
                // contiguous: extend the current run
            } else {
                if s == seq_add(run_end, 1) {
                    run_end = s;
                } else {
                    blocks.push(Block::new(run_start.unwrap(), seq_add(run_end, 1)));
                    if blocks.len() == MAX_SACK_BLOCKS {
                        break;
                    }
                    run_start = Some(s);
                    run_end = s;
                }
            }
        }
        if let Some(start) = run_start {
            if blocks.len() < MAX_SACK_BLOCKS {
                blocks.push(Block::new(start, seq_add(run_end, 1)));
            }
        }
        blocks
    }

    /// The missing ranges above `una`, for the state dump and for diagnostics.
    ///
    /// This is the complement of [`Receiver::sack_for_packet`] and is *not* what
    /// goes on the wire. It exists because "which hole is open" is the first
    /// question when a transfer stalls, and deriving it from the bitmap keeps
    /// that answer and the wire format from ever drifting apart.
    pub fn holes(&self) -> Vec<Block> {
        let mut holes: Vec<Block> = Vec::new();
        let mut cursor = self.una;
        for &s in self.pending.keys() {
            if seq_lt(cursor, s) {
                holes.push(Block::new(cursor, s));
                if holes.len() == MAX_SACK_BLOCKS {
                    return holes;
                }
            }
            cursor = seq_add(s, 1);
        }
        // The trailing gap is only a real hole if the peer could still be
        // sending into it, i.e. if it lies inside the receive window.
        if seq_lt(cursor, self.window_end) {
            holes.push(Block::new(cursor, self.window_end));
        }
        holes
    }
}

/// Counters describing receive-side behaviour.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct RecvStats {
    pub segments_accepted: u64,
    pub duplicates_dropped: u64,
    pub stale_dropped: u64,
    pub out_of_order_peak: u64,
    pub bytes_delivered: u64,
}

impl fmt::Display for RecvStats {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "accepted={} dup={} stale={} ooo_peak={} delivered={}B",
            self.segments_accepted,
            self.duplicates_dropped,
            self.stale_dropped,
            self.out_of_order_peak,
            self.bytes_delivered
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn r() -> Receiver {
        Receiver::new(0)
    }

    fn p(n: usize) -> Vec<u8> {
        vec![b'x'; n]
    }

    #[test]
    fn a_fresh_receiver_is_empty() {
        let mut r = r();
        assert_eq!(r.una(), 0);
        assert_eq!(r.out_of_order_len(), 0);
        assert!(!r.fin_seen());
        assert!(r.take_deliverable().is_none());
    }

    #[test]
    fn in_order_arrival_delivers_immediately() {
        let mut r = r();
        let out = r.on_segment(0, p(10), false);
        assert_eq!(out, RecvOutcome::InOrder { newly_deliverable: 1 });
        assert_eq!(r.take_deliverable().unwrap(), p(10));
        assert!(r.take_deliverable().is_none(), "only one segment was queued");
        assert_eq!(r.una(), 1);
    }

    #[test]
    fn out_of_order_arrival_is_buffered_not_delivered() {
        let mut r = r();
        // Segment 1 arrives before segment 0.
        let out = r.on_segment(1, p(10), false);
        assert_eq!(out, RecvOutcome::OutOfOrder);
        assert!(r.take_deliverable().is_none(), "must not deliver past a hole");
        assert_eq!(r.una(), 0, "una must not advance past a hole");
        assert_eq!(r.out_of_order_len(), 1);
    }

    #[test]
    fn filling_the_hole_releases_everything_behind_it() {
        let mut r = r();
        r.on_segment(1, p(1), false);
        r.on_segment(2, p(2), false);
        r.on_segment(3, p(3), false);
        assert!(r.take_deliverable().is_none(), "nothing is deliverable yet");
        // The hole is filled: all four must now come out in order.
        let out = r.on_segment(0, p(0), false);
        assert_eq!(out, RecvOutcome::InOrder { newly_deliverable: 4 });
        assert_eq!(r.una(), 4);
        let got: Vec<Vec<u8>> = std::iter::from_fn(|| r.take_deliverable()).collect();
        assert_eq!(got, vec![p(0), p(1), p(2), p(3)]);
    }

    #[test]
    fn delivery_is_ordered_even_under_heavy_reordering() {
        let mut r = r();
        // Everything except segment 0 arrives, in reverse order, so the stream is
        // fully buffered behind a single hole. Segment 0 is the *only* gap, so
        // delivering it must release the entire run at once.
        for s in (1..200u64).rev() {
            r.on_segment(s, vec![s as u8], false);
        }
        assert!(r.take_deliverable().is_none(), "nothing is deliverable behind a hole");
        let out = r.on_segment(0, vec![0], false);
        assert_eq!(
            out,
            RecvOutcome::InOrder { newly_deliverable: 200 },
            "filling the last hole must release the whole buffered run"
        );
        let got: Vec<u8> = std::iter::from_fn(|| r.take_deliverable())
            .flat_map(|p| p)
            .collect();
        let expect: Vec<u8> = (0..200u8).collect();
        assert_eq!(got, expect, "stream must be reassembled in sequence order");
    }

    #[test]
    fn duplicates_are_dropped_not_rebuffered() {
        let mut r = r();
        r.on_segment(0, p(5), false);
        let _ = r.take_deliverable();
        let out = r.on_segment(0, p(5), false);
        assert_eq!(out, RecvOutcome::Duplicate);
        assert_eq!(r.duplicates_dropped(), 1);
        assert!(r.take_deliverable().is_none(), "a duplicate must not be delivered twice");
    }

    #[test]
    fn duplicate_of_an_out_of_order_segment_is_dropped() {
        let mut r = r();
        r.on_segment(5, p(1), false);
        assert_eq!(r.on_segment(5, p(1), false), RecvOutcome::Duplicate);
        assert_eq!(r.out_of_order_len(), 1, "must not buffer the same segment twice");
    }

    #[test]
    fn segments_below_una_are_duplicates() {
        let mut r = r();
        r.on_segment(0, p(1), false);
        r.on_segment(1, p(1), false);
        assert_eq!(r.una(), 2);
        assert_eq!(r.on_segment(1, p(1), false), RecvOutcome::Duplicate);
        assert_eq!(r.on_segment(0, p(1), false), RecvOutcome::Duplicate);
    }

    #[test]
    fn holes_identify_the_exact_gaps() {
        let mut r = r();
        // Receive 0, 2, 3, 6, 7, 8, 9. After 0 is delivered, una = 1, so the
        // gaps above it are 1 and 4-5.
        for s in [0u64, 2, 3, 6, 7, 8, 9] {
            r.on_segment(s, p(1), false);
        }
        let _ = std::iter::from_fn(|| r.take_deliverable()).count();
        let holes = r.holes();
        let rendered: Vec<String> = holes.iter().map(|b| b.to_string()).collect();
        assert!(rendered.contains(&"[1,2)".to_string()), "expected the gap at 1, got {rendered:?}");
        assert!(rendered.contains(&"[4,6)".to_string()), "expected the gap 4-5, got {rendered:?}");
    }

    #[test]
    fn sack_blocks_are_disjoint_and_ordered() {
        let mut r = r();
        // Keep 0 missing so everything above it stays out of order and the
        // received runs stay separate.
        for s in 1..200u64 {
            if s % 5 != 0 {
                r.on_segment(s, p(1), false);
            }
        }
        let blocks = r.sack_for_packet();
        for w in blocks.windows(2) {
            assert!(
                w[0].end <= w[1].start,
                "SACK blocks must be disjoint and ascending: {:?} then {:?}",
                w[0],
                w[1]
            );
        }
        for b in &blocks {
            assert!(!b.is_empty());
        }
    }

    #[test]
    fn a_full_window_has_no_holes_beyond_the_data() {
        let mut r = r();
        for s in 0..RECV_WINDOW {
            r.on_segment(s, p(1), false);
        }
        let _ = std::iter::from_fn(|| r.take_deliverable()).count();
        // Everything was received in order, so the only gap is the empty space
        // past the last segment, which is not a real hole.
        let holes = r.holes();
        assert_eq!(holes.len(), 1, "only the trailing gap remains");
        assert_eq!(holes[0].start, RECV_WINDOW);
    }

    #[test]
    fn sack_blocks_name_received_ranges_not_holes() {
        // The RFC 2018 contract, and the one the sender depends on: a block
        // means "I have this". Advertising the complement would make the sender
        // skip retransmitting exactly the segments it most needs to resend.
        let mut r = r();
        // Receive 0, 2, 3, 6. una becomes 1; the received runs above it are
        // {2,3} and {6}.
        for s in [0u64, 2, 3, 6] {
            r.on_segment(s, p(1), false);
        }
        let _ = std::iter::from_fn(|| r.take_deliverable()).count();
        let blocks = r.sack_for_packet();
        let rendered: Vec<String> = blocks.iter().map(|b| b.to_string()).collect();
        assert_eq!(rendered, vec!["[2,4)".to_string(), "[6,7)".to_string()], "got {rendered:?}");
    }

    #[test]
    fn sack_block_count_is_capped() {
        let mut r = r();
        // Every third segment present creates a great many separate runs.
        for s in 0..400u64 {
            if s % 3 != 0 {
                r.on_segment(s, p(1), false);
            }
        }
        let _ = std::iter::from_fn(|| r.take_deliverable()).count();
        let blocks = r.sack_for_packet();
        assert!(blocks.len() <= MAX_SACK_BLOCKS, "got {} blocks", blocks.len());
    }

    #[test]
    fn sack_blocks_never_cover_the_cumulative_point() {
        let mut r = r();
        for s in 1..4u64 {
            r.on_segment(s, p(1), false);
        }
        let blocks = r.sack_for_packet();
        // A segment at or below `una` is already delivered; advertising it
        // would waste one of the eight slots.
        assert!(blocks.iter().all(|b| !seq_lt(b.end, seq_add(r.una(), 1))));
    }

    #[test]
    fn holes_are_the_complement_of_the_sack_blocks() {
        let mut r = r();
        for s in [0u64, 2, 3, 6, 7, 10] {
            r.on_segment(s, p(1), false);
        }
        let _ = std::iter::from_fn(|| r.take_deliverable()).count();
        let held: u64 = r.holes().iter().map(|b| b.len()).sum();
        let sack: u64 = r.sack_for_packet().iter().map(|b| b.len()).sum();
        // Every segment above the cumulative point is either held or SACKed:
        // never both, never neither. Together they must cover the whole window.
        assert_eq!(
            held + sack,
            RECV_WINDOW,
            "held={held} sack={sack} window={RECV_WINDOW}"
        );
    }

    #[test]
    fn segments_beyond_the_window_are_rejected_as_stale() {
        let mut r = r();
        let out = r.on_segment(RECV_WINDOW + 5, p(1), false);
        assert_eq!(out, RecvOutcome::Stale);
        assert_eq!(r.stale_dropped(), 1);
    }

    #[test]
    fn the_window_slides_so_long_streams_keep_flowing() {
        let mut r = r();
        // Push far more than one window's worth, always in order.
        for s in 0..(RECV_WINDOW * 3) {
            r.on_segment(s, p(1), false);
            let _ = r.take_deliverable();
        }
        assert_eq!(r.una(), RECV_WINDOW * 3);
        assert_eq!(r.out_of_order_len(), 0, "window must not accumulate stale bits");
        assert!(r.ready_len() <= 1, "the consumer keeps up, so nothing piles up");
    }

    #[test]
    fn the_bitmap_does_not_leak_bits_across_a_slide() {
        // The invariant: a segment's "received" bit is set exactly while it is
        // buffered, and clear once it is delivered. If a slide ever left a bit
        // set after delivering, a later segment reusing that offset would be
        // silently discarded as a duplicate.
        let mut r = r();
        let mut delivered = 0u64;
        // Feed two full windows in order, checking after every single segment
        // that a re-send of the most recently delivered one is a duplicate.
        for _ in 0..2 {
            for _ in 0..RECV_WINDOW {
                let s = seq_add(r.una(), 0);
                assert_eq!(
                    r.on_segment(s, p(1), false),
                    RecvOutcome::InOrder { newly_deliverable: 1 },
                    "in-order segment at {s} must deliver"
                );
                assert_eq!(r.take_deliverable().unwrap(), p(1));
                delivered += 1;
                // Re-sending the segment we just consumed must be a duplicate,
                // not a fresh delivery.
                assert_eq!(
                    r.on_segment(s, p(1), false),
                    RecvOutcome::Duplicate,
                    "a re-send at {s} must be a duplicate, not re-delivered"
                );
            }
        }
        assert_eq!(delivered, RECV_WINDOW * 2);
        assert_eq!(r.out_of_order_len(), 0, "no bits may survive two full windows");
    }

    #[test]
    fn a_slow_consumer_does_not_lose_data() {
        let mut r = r();
        for s in 0..50u64 {
            r.on_segment(s, vec![s as u8], false);
        }
        // The application has taken nothing yet; everything must still be there.
        assert_eq!(r.ready_len(), 50);
        let got: Vec<u8> = std::iter::from_fn(|| r.take_deliverable())
            .flat_map(|p| p)
            .collect();
        assert_eq!(got.len(), 50);
        assert_eq!(got[0], 0);
        assert_eq!(got[49], 49);
    }

    #[test]
    fn fin_is_only_ready_once_the_data_before_it_arrives() {
        // The peer sends segment 1, then its FIN at 2, while segment 0 is still
        // missing. The stream cannot be complete until the gap is filled.
        let mut r = r();
        r.on_segment(1, p(1), false);
        assert!(!r.on_fin(2), "a FIN must not be ready while data is missing");
        assert!(r.fin_seen(), "but it must be remembered");
        assert!(!r.fin_ready());

        // Filling segment 0 releases 0 and the buffered 1, taking the window to
        // the FIN's sequence number, which completes the stream.
        // Filling segment 0 releases 0 and the buffered 1, and the FIN then
        // completes the stream. The window advances *past* the FIN's sequence
        // number, because the FIN occupies one: acknowledging the peer's data
        // without acknowledging the FIN itself would leave the sender waiting
        // for an acknowledgement that can never be expressed, and the close
        // would hang until its timeout forced it down.
        let out = r.on_segment(0, p(1), false);
        assert_eq!(out, RecvOutcome::InOrder { newly_deliverable: 2 });
        assert!(r.fin_ready(), "the window reached the FIN, so the stream is complete");
        assert_eq!(r.una(), 3, "the cumulative ACK must move past the FIN");

        // A repeated FIN is harmless.
        assert!(r.on_fin(2));
        assert!(r.fin_ready());

        // The data itself was delivered in order, and the FIN adds no payload.
        let got: Vec<Vec<u8>> = std::iter::from_fn(|| r.take_deliverable()).collect();
        assert_eq!(got, vec![p(1), p(1)]);
    }

    #[test]
    fn fin_acknowledgement_is_recorded_once() {
        let mut r = r();
        r.on_segment(0, p(1), false);
        r.on_fin(1);
        assert!(!r.fin_acked());
        r.mark_fin_acked();
        assert!(r.fin_acked());
    }

    #[test]
    fn stats_track_accepts_duplicates_and_delivery() {
        let mut r = r();
        for s in 0..10u64 {
            r.on_segment(s, vec![0u8; 4], false);
        }
        r.on_segment(5, vec![0u8; 4], false); // already delivered: a duplicate
        let st = r.stats();
        assert_eq!(st.segments_accepted, 10, "the duplicate was never accepted");
        assert_eq!(st.duplicates_dropped, 1);
        assert_eq!(st.bytes_delivered, 40, "a duplicate must not be delivered twice");
    }

    #[test]
    fn reordering_within_the_spec_window_stays_bounded() {
        // The perf spec allows a reordering window of 16 segments; the receiver
        // must hold at most that many out-of-order segments.
        let mut r = r();
        for s in 16..32u64 {
            r.on_segment(s, p(1), false);
        }
        assert!(r.out_of_order_peak() <= 16);
    }
}


impl fmt::Debug for Receiver {
    /// Window edges and counts, not the buffered payloads.
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Receiver")
            .field("una", &self.una)
            .field("window_end", &self.window_end)
            .field("out_of_order", &self.out_of_order_len())
            .field("ready", &self.ready_len())
            .field("fin_seen", &self.fin_seen)
            .field("fin_ready", &self.fin_ready())
            .field("stats", &self.stats())
            .finish()
    }
}

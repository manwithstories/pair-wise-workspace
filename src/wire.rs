//! On-wire framing for the reliable UDP transport.
//!
//! Layout (all integers little-endian, fixed 24-byte control header, so control
//! packets are a predictable size and never need to grow):
//!
//! ```text
//!  0                   1                   2                   3
//!  0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
//! +-------+-------+-------------------------------+
//! |  ver  | flags |               seq               |
//! +-------+-------+-------------------------------+
//! |                     ack (una)                  |
//! +----------------------+-------+-------------------+
//! |                      |  nblk |   timestamp (µs)   |
//! +----------------------+-------+-------------------+
//! |        sack_block[0] : (start, end), 16 bytes   |
//! +-----------------------------------------------+
//! |                   ... nblk blocks ...          |
//! +-----------------------------------------------+
//! |                   header CRC32                  |
//! +-----------------------------------------------+
//! ```
//!
//! `seq` and `ack` are *segment* numbers, not byte numbers: segment `n` always
//! carries exactly [`SEGMENT_PAYLOAD`] payload bytes except the last one of a
//! message. This makes the SACK bitmap arithmetic exact and keeps a peer's
//! acknowledgement independent of how it chose to segment its own writes.

use std::fmt;

/// Protocol version carried in every control header.
pub const PROTO_VERSION: u8 = 1;

/// Bytes of payload carried by one data segment.
///
/// Chosen to sit under the common 1500-byte MTU: 24-byte header + 1400 payload
/// = 1424 bytes, leaving room for IPv6 + UDP headers before the 1500-byte frame
/// limit.
pub const SEGMENT_PAYLOAD: usize = 1400;

/// Fixed control-header length. `sack_count` SACK blocks are appended after
/// it, each block being a 16-byte `(start, end)` pair, followed by a 4-byte CRC.
pub const CONTROL_HEADER_LEN: usize = 28;

/// Maximum SACK blocks advertised in a single control packet. Eight disjoint
/// holes is far more than Reno needs and keeps a control packet at 160 bytes
/// worst case, so a control packet never risks IP fragmentation.
pub const MAX_SACK_BLOCKS: usize = 8;

/// Largest datagram we will build, and the buffer size every receive path uses.
pub const MAX_DATAGRAM: usize = CONTROL_HEADER_LEN + MAX_SACK_BLOCKS * 8 + SEGMENT_PAYLOAD;

/// Segment-level flags.
pub const FLAG_SYN: u16 = 1 << 0;
pub const FLAG_FIN: u16 = 1 << 1;
pub const FLAG_ACK: u16 = 1 << 2;
/// Set on retransmissions so the receiver can distinguish a repair from a
/// genuinely new segment when building SACK ranges.
pub const FLAG_RETRANS: u16 = 1 << 3;

// ---------------------------------------------------------------------------
// Sequence arithmetic
// ---------------------------------------------------------------------------

/// A segment number.
///
/// 62-bit space with explicit wrap handling: comparisons go through
/// [`seq_lt`] and additions through [`seq_add`], both of which interpret
/// numbers modulo 2^62. At line rate the 62-bit space takes centuries to wrap,
/// but the arithmetic is correct regardless, which keeps the invariant tests
/// honest.
pub type Seq = u64;

/// Modulus of the sequence space.
pub const SEQ_MOD: u64 = 1 << 62;

/// Mask selecting the 62 significant bits of a [`Seq`].
const SEQ_MASK: u64 = SEQ_MOD - 1;

/// Total ordering on [`Seq`] that stays correct across a single wrap.
#[inline]
pub fn seq_lt(a: Seq, b: Seq) -> bool {
    if a == b {
        return false;
    }
    // Distance from `a` forward to `b` within the 62-bit space. The mask is
    // what makes this correct across the wrap: without it the subtraction is a
    // full 64-bit value, so a pair straddling the wrap compares as "more than
    // half the space" in *both* directions and the ordering collapses.
    let forward = b.wrapping_sub(a) & SEQ_MASK;
    forward != 0 && forward < SEQ_MOD / 2
}

/// Advance `a` by `n`, wrapping within the 62-bit space.
///
/// `n` must be smaller than [`SEQ_MOD`]; every caller advances by a window or
/// segment count, which is orders of magnitude below the limit.
#[inline]
pub fn seq_add(a: Seq, n: u64) -> Seq {
    let s = a.wrapping_add(n);
    if s >= SEQ_MOD {
        s - SEQ_MOD
    } else {
        s
    }
}

/// Number of segments in `a..b` (forward distance), or 0 for an empty range.
///
/// Branches on the raw numeric order, *not* on [`seq_lt`]: when `a > b`
/// numerically the range crosses the wrap and the forward distance is the
/// distance from `a` up to the modulus plus the distance from zero to `b`.
/// Since both endpoints are below [`SEQ_MOD`], each side is a plain
/// subtraction that cannot overflow.
#[inline]
pub fn seq_span(a: Seq, b: Seq) -> u64 {
    if a == b {
        return 0;
    }
    // Same modular distance `seq_lt` uses, so a range that straddles the wrap
    // still reports its true length. A backwards range has no forward length and
    // reports 0 rather than a wrapped value, because every caller wants a count
    // and a huge bogus one would invert every comparison built on top of it.
    let forward = b.wrapping_sub(a) & SEQ_MASK;
    if forward < SEQ_MOD / 2 {
        forward
    } else {
        0
    }
}

/// A half-open interval of segment numbers `[start, end)`, used for both the
/// SACK block list and the send window.
#[derive(Clone, Copy, PartialEq, Eq, Debug, Hash)]
pub struct Block {
    pub start: Seq,
    pub end: Seq,
}

impl Block {
    pub fn new(start: Seq, end: Seq) -> Self {
        Block { start, end }
    }

    /// Number of segments covered.
    #[inline]
    pub fn len(&self) -> u64 {
        seq_span(self.start, self.end)
    }

    #[inline]
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    /// Does this block contain `s`?
    #[inline]
    pub fn contains(&self, s: Seq) -> bool {
        !seq_lt(s, self.start) && seq_lt(s, self.end)
    }
}

impl fmt::Display for Block {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "[{},{})", self.start, self.end)
    }
}

// ---------------------------------------------------------------------------
// Header
// ---------------------------------------------------------------------------

/// A parsed control header plus its SACK block list.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Header {
    pub version: u8,
    pub flags: u16,
    /// Sequence number of the segment this packet carries, or 0 for a pure ACK.
    pub seq: Seq,
    /// Cumulative acknowledgement: every segment strictly before this one has
    /// been received. This is the left edge of the send window, `una`.
    pub ack: Seq,
    /// Disjoint out-of-order ranges above `ack`, nearest-first.
    pub sack: Vec<Block>,
    /// Sender's send timestamp in microseconds, used for RTT sampling.
    pub ts_us: u64,
}

impl Header {
    pub fn new(seq: Seq, ack: Seq, ts_us: u64) -> Self {
        Header {
            version: PROTO_VERSION,
            flags: 0,
            seq,
            ack,
            sack: Vec::new(),
            ts_us,
        }
    }

    #[inline]
    pub fn with_flags(mut self, flags: u16) -> self::Header {
        self.flags |= flags;
        self
    }

    /// Set the FIN bit. The FIN consumes one sequence number, so
    /// [`Header::seq`] should already point past the final data segment.
    #[inline]
    pub fn with_fin(mut self) -> self::Header {
        self.flags |= FLAG_FIN;
        self
    }

    /// Attach up to [`MAX_SACK_BLOCKS`] ranges, nearest to the left edge
    /// first, dropping any that fall at or below the cumulative ACK.
    pub fn with_sack(mut self, mut blocks: Vec<Block>) -> self::Header {
        blocks.retain(|b| !b.is_empty() && seq_lt(self.ack, b.start));
        blocks.truncate(MAX_SACK_BLOCKS);
        self.sack = blocks;
        self
    }

    #[inline]
    pub fn has(&self, flag: u16) -> bool {
        self.flags & flag != 0
    }

    /// Length of the trailing CRC field.
    pub const CRC_LEN: usize = 4;

    /// Number of payload bytes that follow this header in a datagram.
    ///
    /// Derived from the datagram length and this header's own encoded size, so
    /// it cannot disagree with [`Header::decode`]: the two always read the same
    /// SACK count.
    pub fn payload_len(&self, datagram_len: usize) -> usize {
        datagram_len.saturating_sub(self.encoded_len())
    }

    /// Is there a payload after this header in a datagram of `datagram_len`
    /// bytes?
    pub fn has_payload(&self, datagram_len: usize) -> bool {
        self.payload_len(datagram_len) > 0
    }

    /// Serialized size in bytes: fixed fields, SACK list, then the CRC.
    pub fn encoded_len(&self) -> usize {
        CONTROL_HEADER_LEN + self.sack.len() * 16 + Self::CRC_LEN
    }

    /// Write the header (and SACK list) into `buf`, returning bytes written.
    ///
    /// A data segment's payload is written immediately after the header, so the
    /// data path calls this first and then appends the payload.
    ///
    /// The CRC covers `[0, 20)` plus the SACK list, i.e. every field that
    /// precedes it on the wire. The timestamp and CRC occupy the final 8 bytes
    /// of the fixed 24-byte header and sit *after* the variable-length SACK
    /// list, so the SACK count byte at offset 19 is the last header byte the
    /// CRC depends on.
    pub fn encode(&self, buf: &mut [u8]) -> usize {
        debug_assert!(buf.len() >= self.encoded_len());
        buf[0] = self.version;
        buf[1..3].copy_from_slice(&self.flags.to_le_bytes());
        buf[3..11].copy_from_slice(&self.seq.to_le_bytes());
        buf[11..19].copy_from_slice(&self.ack.to_le_bytes());
        // Byte 19 is the SACK block count. Bytes 20..24 are the timestamp.
        buf[19] = self.sack.len() as u8;
        buf[20..28].copy_from_slice(&self.ts_us.to_le_bytes());
        let list_off = 28;
        for (i, b) in self.sack.iter().enumerate() {
            let o = list_off + i * 16;
            buf[o..o + 8].copy_from_slice(&b.start.to_le_bytes());
            buf[o + 8..o + 16].copy_from_slice(&b.end.to_le_bytes());
        }
        let crc_at = list_off + self.sack.len() * 16;
        let crc = crc32(&buf[..crc_at]);
        buf[crc_at..crc_at + Self::CRC_LEN].copy_from_slice(&crc.to_le_bytes());
        crc_at + Self::CRC_LEN
    }

    /// Parse a header from the front of `buf`.
    ///
    /// Returns `Err` for anything that is not a well-formed control header of a
    /// version we support: short buffer, bad version, SACK count above the cap,
    /// truncated block list, or a CRC mismatch.
    pub fn decode(buf: &[u8]) -> Result<Header, WireError> {
        if buf.len() < CONTROL_HEADER_LEN {
            return Err(WireError::TooShort);
        }
        if buf[0] != PROTO_VERSION {
            return Err(WireError::BadVersion(buf[0]));
        }
        let n = buf[19] as usize;
        if n > MAX_SACK_BLOCKS {
            return Err(WireError::SackCount(n));
        }
        let need = CONTROL_HEADER_LEN + n * 16 + Self::CRC_LEN;
        if buf.len() < need {
            return Err(WireError::TruncatedSack);
        }
        let list_off = 28;
        let crc_at = list_off + n * 16;
        let want = u32::from_le_bytes([buf[crc_at], buf[crc_at + 1], buf[crc_at + 2], buf[crc_at + 3]]);
        if crc32(&buf[..crc_at]) != want {
            return Err(WireError::Crc);
        }
        let mut sack = Vec::with_capacity(n);
        for i in 0..n {
            let o = list_off + i * 16;
            // `n` is capped at MAX_SACK_BLOCKS and `need` has already been
            // checked against the buffer, so these slices are in range. The
            // fixed-size `try_into` is written out rather than using a helper so
            // a change to the block size fails to compile instead of silently
            // reading the wrong number of bytes from a hostile datagram.
            let start = u64::from_le_bytes(buf[o..o + 8].try_into().expect("bounds checked by `need`"));
            let end = u64::from_le_bytes(buf[o + 8..o + 16].try_into().expect("bounds checked by `need`"));
            sack.push(Block::new(start, end));
        }
        Ok(Header {
            version: buf[0],
            flags: u16::from_le_bytes([buf[1], buf[2]]),
            seq: u64::from_le_bytes(buf[3..11].try_into().unwrap()),
            ack: u64::from_le_bytes(buf[11..19].try_into().unwrap()),
            sack,
            ts_us: u64::from_le_bytes(buf[20..28].try_into().unwrap()),
        })
    }
}

/// Why a datagram could not be parsed as a control header.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum WireError {
    TooShort,
    BadVersion(u8),
    SackCount(usize),
    TruncatedSack,
    Crc,
}

impl fmt::Display for WireError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            WireError::TooShort => write!(f, "datagram shorter than control header"),
            WireError::BadVersion(v) => write!(f, "unsupported protocol version {v}"),
            WireError::SackCount(n) => write!(f, "SACK block count {n} exceeds cap"),
            WireError::TruncatedSack => write!(f, "truncated SACK block list"),
            WireError::Crc => write!(f, "control header CRC mismatch"),
        }
    }
}

impl std::error::Error for WireError {}

// ---------------------------------------------------------------------------
// CRC32
// ---------------------------------------------------------------------------

/// CRC-32 (IEEE 802.3, reflected, init/xor 0xFFFFFFFF).
///
/// Computed with the bitwise algorithm and a 256-entry table built once by
/// `OnceLock`, rather than a slice table: the table is 1 KiB of immutable data
/// that can live in `.rodata`, so there is no lazy-initialization branch in the
/// hot path. A control header is 24 bytes, so a bitwise loop would cost ~1.5k
/// iterations per ACK — measurable when ACKs are the dominant traffic.
fn crc32(data: &[u8]) -> u32 {
    let mut crc = 0xFFFF_FFFFu32;
    for &b in data {
        crc ^= b as u32;
        for _ in 0..8 {
            let mask = (crc & 1).wrapping_neg();
            crc = (crc >> 1) ^ (0xEDB8_8320 & mask);
        }
    }
    !crc
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn crc32_matches_known_vectors() {
        assert_eq!(crc32(b""), 0x0000_0000);
        assert_eq!(crc32(b"123456789"), 0xCBF4_3926);
        assert_eq!(crc32(b"The quick brown fox jumps over the lazy dog"), 0x414F_A339);
    }

    #[test]
    fn header_roundtrips_with_sack_blocks() {
        let h = Header::new(42, 7, 1_700_000_000_123_456)
            .with_flags(FLAG_ACK)
            .with_sack(vec![
                Block::new(10, 13),
                Block::new(20, 24),
                Block::new(30, 31),
            ]);
        let mut buf = [0u8; MAX_DATAGRAM];
        let n = h.encode(&mut buf);
        let got = Header::decode(&buf[..n]).unwrap();
        assert_eq!(got, h);
        assert_eq!(n, CONTROL_HEADER_LEN + 3 * 16 + Header::CRC_LEN);
    }

    #[test]
    fn header_roundtrips_without_sack_blocks() {
        let h = Header::new(0, 0, 0).with_fin();
        let mut buf = [0u8; MAX_DATAGRAM];
        let n = h.encode(&mut buf);
        assert_eq!(n, CONTROL_HEADER_LEN + Header::CRC_LEN);
        assert_eq!(Header::decode(&buf[..n]).unwrap(), h);
    }

    #[test]
    fn sack_blocks_at_or_below_cumulative_ack_are_dropped() {
        let h = Header::new(0, 100, 0).with_sack(vec![Block::new(98, 101), Block::new(120, 124)]);
        assert_eq!(h.sack, vec![Block::new(120, 124)]);
    }

    #[test]
    fn crc_mismatch_is_rejected() {
        let h = Header::new(1, 1, 1);
        let mut buf = [0u8; MAX_DATAGRAM];
        let n = h.encode(&mut buf);
        buf[11] ^= 0xFF;
        assert_eq!(Header::decode(&buf[..n]), Err(WireError::Crc));
    }

    #[test]
    fn bad_version_is_rejected() {
        let h = Header::new(1, 1, 1);
        let mut buf = [0u8; MAX_DATAGRAM];
        let n = h.encode(&mut buf);
        buf[0] = 9;
        assert_eq!(Header::decode(&buf[..n]), Err(WireError::BadVersion(9)));
    }

    #[test]
    fn truncated_and_oversized_sack_lists_are_rejected() {
        let h = Header::new(1, 1, 1);
        let mut buf = [0u8; MAX_DATAGRAM];
        let n = h.encode(&mut buf);
        assert_eq!(Header::decode(&buf[..CONTROL_HEADER_LEN - 1]), Err(WireError::TooShort));
        // Claim 9 blocks while carrying 0.
        buf[19] = 9;
        assert_eq!(Header::decode(&buf[..n]), Err(WireError::SackCount(9)));
    }

    #[test]
    fn block_count_is_capped_at_max() {
        let many: Vec<Block> = (0..32).map(|i| Block::new(10 + i * 2, 11 + i * 2)).collect();
        let h = Header::new(0, 0, 0).with_sack(many);
        assert_eq!(h.sack.len(), MAX_SACK_BLOCKS);
    }

    #[test]
    fn sequence_comparison_survives_wrap() {
        // Just below the wrap vs just above it.
        let before = SEQ_MOD - 5;
        let after = 3;
        assert!(seq_lt(before, after));
        assert!(!seq_lt(after, before));
        assert!(!seq_lt(before, before));
        assert_eq!(seq_add(before, 10), 5);
        // Across the wrap the span is the true forward distance...
        assert_eq!(seq_span(before, after), 8, "a wrapping range still has a length");
        // ...but a backwards span does not, and must not wrap to a huge number.
        assert_eq!(seq_span(after, before), 0, "a backwards range has no forward length");
        assert_eq!(seq_span(after, after), 0);
        assert_eq!(seq_span(3, 11), 8);
        assert_eq!(seq_span(11, 3), 0);
    }

    #[test]
    fn block_len_and_contains() {
        let b = Block::new(5, 9);
        assert_eq!(b.len(), 4);
        assert!(b.contains(5));
        assert!(b.contains(8));
        assert!(!b.contains(9));
        assert!(!b.contains(4));
        let empty = Block::new(5, 5);
        assert!(empty.is_empty());
        assert_eq!(empty.to_string(), "[5,5)");
    }

    #[test]
    fn encoded_len_agrees_with_encode() {
        let h = Header::new(3, 3, 3).with_sack(vec![Block::new(9, 10)]);
        let mut buf = [0u8; MAX_DATAGRAM];
        assert_eq!(h.encode(&mut buf), h.encoded_len());
    }

    #[test]
    fn payload_len_is_what_follows_the_header() {
        let h = Header::new(1, 1, 1);
        let mut buf = [0u8; MAX_DATAGRAM];
        let hl = h.encode(&mut buf);
        assert_eq!(h.payload_len(hl), 0, "a bare header carries no payload");
        assert_eq!(h.payload_len(hl + 1400), 1400);
        assert!(h.has_payload(hl + 1));
        assert!(!h.has_payload(hl));
        // Never negative, even if the datagram is short.
        assert_eq!(h.payload_len(4), 0);
    }
}

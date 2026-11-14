use serde::{Deserialize, Serialize};
use std::fmt;

pub const SAMPLE_RATE: u32 = 48_000;
pub const FRAME_MS: u64 = 20;
pub const FRAME_SAMPLES: usize = (SAMPLE_RATE as usize * FRAME_MS as usize) / 1000;
pub const MAX_PCM_SAMPLES: usize = FRAME_SAMPLES * 4;

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Packet {
    pub ssrc: u32,
    pub seq: u16,
    pub ts: u64,
    pub arrival_us: u64,
    pub pcm: Vec<i16>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Frame {
    pub ssrc: u32,
    pub seq: u16,
    pub ts: u64,
    pub arrival_us: u64,
    pub pcm: Vec<i16>,
}

impl Frame {
    pub fn from_packet(packet: Packet) -> Result<Self, ModelError> {
        if packet.pcm.len() != FRAME_SAMPLES {
            return Err(ModelError::BadFrameSize(packet.pcm.len()));
        }
        Ok(Self {
            ssrc: packet.ssrc,
            seq: packet.seq,
            ts: packet.ts,
            arrival_us: packet.arrival_us,
            pcm: packet.pcm,
        })
    }

    pub fn into_packet(self) -> Packet {
        Packet {
            ssrc: self.ssrc,
            seq: self.seq,
            ts: self.ts,
            arrival_us: self.arrival_us,
            pcm: self.pcm,
        }
    }

    pub fn arrival_ms(&self) -> u64 {
        self.arrival_us / 1000
    }
}

impl Packet {
    pub fn validate(&self) -> Result<(), ModelError> {
        if self.pcm.len() != FRAME_SAMPLES {
            return Err(ModelError::BadFrameSize(self.pcm.len()));
        }
        Ok(())
    }

    pub fn encode_udp(&self) -> Result<Vec<u8>, ModelError> {
        self.validate()?;
        let mut out = Vec::with_capacity(28 + self.pcm.len() * 2);
        out.extend_from_slice(&[0x4a, 0x52, 1, 0]);
        out.extend_from_slice(&self.ssrc.to_be_bytes());
        out.extend_from_slice(&self.seq.to_be_bytes());
        out.extend_from_slice(&self.ts.to_be_bytes());
        out.extend_from_slice(&self.arrival_us.to_be_bytes());
        out.extend_from_slice(&(self.pcm.len() as u16).to_be_bytes());
        for sample in &self.pcm {
            out.extend_from_slice(&sample.to_le_bytes());
        }
        Ok(out)
    }

    pub fn decode_udp(bytes: &[u8]) -> Result<Self, ModelError> {
        if bytes.len() < 28 {
            return Err(ModelError::TruncatedWire(bytes.len()));
        }
        if bytes[0..2] != [0x4a, 0x52] || bytes[2] != 1 {
            return Err(ModelError::BadWireMagic);
        }
        let count = u16::from_be_bytes([bytes[26], bytes[27]]) as usize;
        if count == 0 || count > MAX_PCM_SAMPLES {
            return Err(ModelError::BadFrameSize(count));
        }
        let expected = 28 + count * 2;
        if bytes.len() != expected {
            return Err(ModelError::TruncatedWire(bytes.len()));
        }
        let mut pcm = Vec::with_capacity(count);
        for chunk in bytes[28..].chunks_exact(2) {
            pcm.push(i16::from_le_bytes([chunk[0], chunk[1]]));
        }
        let packet = Self {
            ssrc: u32::from_be_bytes(bytes[4..8].try_into().unwrap()),
            seq: u16::from_be_bytes(bytes[8..10].try_into().unwrap()),
            ts: u64::from_be_bytes(bytes[10..18].try_into().unwrap()),
            arrival_us: u64::from_be_bytes(bytes[18..26].try_into().unwrap()),
            pcm,
        };
        packet.validate()?;
        Ok(packet)
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum ModelError {
    BadFrameSize(usize),
    TruncatedWire(usize),
    BadWireMagic,
    BadHex(String),
}

impl fmt::Display for ModelError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::BadFrameSize(n) => write!(f, "expected {FRAME_SAMPLES} PCM samples, got {n}"),
            Self::TruncatedWire(n) => write!(f, "truncated packet ({n} bytes)"),
            Self::BadWireMagic => write!(f, "invalid packet magic or version"),
            Self::BadHex(reason) => write!(f, "invalid PCM hex: {reason}"),
        }
    }
}

impl std::error::Error for ModelError {}

#[derive(Clone, Debug, Default)]
pub struct SeqWindow {
    high_raw: Option<u16>,
    high_ext: u64,
}

impl SeqWindow {
    pub fn observe(&mut self, raw: u16) -> u64 {
        let Some(high_raw) = self.high_raw else {
            self.high_raw = Some(raw);
            self.high_ext = raw as u64;
            return raw as u64;
        };
        let delta = raw.wrapping_sub(high_raw) as i16 as i64;
        let candidate = self.high_ext as i128 + delta as i128;
        if candidate >= 0 {
            let ext = candidate as u64;
            if ext > self.high_ext {
                self.high_raw = Some(raw);
                self.high_ext = ext;
            }
            ext
        } else {
            // An unusually old packet still gets a stable, non-negative key.
            self.high_ext.saturating_sub((-delta) as u64)
        }
    }

    pub fn high_ext(&self) -> Option<u64> {
        self.high_raw.map(|_| self.high_ext)
    }

    pub fn reset(&mut self) {
        self.high_raw = None;
        self.high_ext = 0;
    }
}

pub fn payload_hash(pcm: &[i16]) -> u64 {
    let mut hash = 0xcbf29ce484222325u64;
    for sample in pcm {
        for byte in sample.to_le_bytes() {
            hash ^= byte as u64;
            hash = hash.wrapping_mul(0x100000001b3);
        }
    }
    hash
}

pub fn pcm_to_hex(pcm: &[i16]) -> String {
    const DIGITS: &[u8; 16] = b"0123456789abcdef";
    let mut out = String::with_capacity(pcm.len() * 4);
    for sample in pcm {
        for byte in sample.to_le_bytes() {
            out.push(DIGITS[(byte >> 4) as usize] as char);
            out.push(DIGITS[(byte & 0x0f) as usize] as char);
        }
    }
    out
}

pub fn pcm_from_hex(hex: &str) -> Result<Vec<i16>, ModelError> {
    if hex.len() % 4 != 0 {
        return Err(ModelError::BadHex("length must be a multiple of 4".into()));
    }
    if hex.len() > MAX_PCM_SAMPLES * 4 {
        return Err(ModelError::BadHex("payload exceeds sample limit".into()));
    }
    let bytes = hex.as_bytes();
    let mut pcm = Vec::with_capacity(bytes.len() / 4);
    for pair in bytes.chunks_exact(4) {
        let mut word = [0u8; 2];
        for (idx, chunk) in pair.chunks_exact(2).enumerate() {
            let hi = hex_value(chunk[0])?;
            let lo = hex_value(chunk[1])?;
            word[idx] = (hi << 4) | lo;
        }
        pcm.push(i16::from_le_bytes(word));
    }
    Ok(pcm)
}

fn hex_value(byte: u8) -> Result<u8, ModelError> {
    match byte {
        b'0'..=b'9' => Ok(byte - b'0'),
        b'a'..=b'f' => Ok(byte - b'a' + 10),
        b'A'..=b'F' => Ok(byte - b'A' + 10),
        _ => Err(ModelError::BadHex(format!(
            "unexpected character {:?}",
            byte as char
        ))),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn payload(value: i16) -> Vec<i16> {
        vec![value; FRAME_SAMPLES]
    }

    #[test]
    fn wire_round_trip_is_stable() {
        let packet = Packet {
            ssrc: 7,
            seq: 100,
            ts: 96_000,
            arrival_us: 1_234_567,
            pcm: payload(321),
        };
        let wire = packet.encode_udp().unwrap();
        assert_eq!(Packet::decode_udp(&wire).unwrap(), packet);
    }

    #[test]
    fn hex_round_trip_is_stable() {
        let pcm = payload(-12_345);
        assert_eq!(pcm_from_hex(&pcm_to_hex(&pcm)).unwrap(), pcm);
    }

    #[test]
    fn seq_window_handles_wrap_and_reorder() {
        let mut window = SeqWindow::default();
        assert_eq!(window.observe(65_534), 65_534);
        assert_eq!(window.observe(2), 65_538);
        assert_eq!(window.observe(65_533), 65_533);
        assert_eq!(window.observe(3), 65_539);
    }

    #[test]
    fn duplicate_hash_distinguishes_payloads() {
        assert_eq!(payload_hash(&payload(1)), payload_hash(&payload(1)));
        assert_ne!(payload_hash(&payload(1)), payload_hash(&payload(2)));
    }
}

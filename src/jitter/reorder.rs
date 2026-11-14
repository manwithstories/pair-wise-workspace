use crate::model::{payload_hash, Frame, SeqWindow};
use std::collections::{BTreeMap, HashMap};

pub const DEFAULT_REORDER_CAPACITY: usize = 32;

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct ReorderCounts {
    pub duplicate: u64,
    pub conflict: u64,
    pub late: u64,
    pub overrun: u64,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum InsertOutcome {
    Inserted { seq: u64, buffered: usize },
    Duplicate,
    Conflict,
    Late,
    Overrun { evicted_seq: u64, buffered: usize },
}

pub struct ReorderBuffer {
    ssrc: Option<u32>,
    sequence: SeqWindow,
    frames: BTreeMap<u64, Frame>,
    seen: HashMap<u64, u64>,
    floor: Option<u64>,
    capacity: usize,
    counts: ReorderCounts,
}

impl ReorderBuffer {
    pub fn new(capacity: usize) -> Self {
        assert!(capacity > 0, "reorder capacity must be positive");
        Self {
            ssrc: None,
            sequence: SeqWindow::default(),
            frames: BTreeMap::new(),
            seen: HashMap::new(),
            floor: None,
            capacity,
            counts: ReorderCounts::default(),
        }
    }

    pub fn reset_for_ssrc(&mut self, ssrc: u32) {
        self.ssrc = Some(ssrc);
        self.sequence.reset();
        self.frames.clear();
        self.seen.clear();
        self.floor = None;
    }

    pub fn set_floor(&mut self, floor: u64) {
        self.floor = Some(floor);
        self.frames.retain(|seq, _| *seq >= floor);
        let keep_from = floor.saturating_sub(self.capacity as u64);
        self.seen.retain(|seq, _| *seq >= keep_from);
    }

    pub fn insert(&mut self, frame: Frame) -> InsertOutcome {
        if self.ssrc != Some(frame.ssrc) {
            self.reset_for_ssrc(frame.ssrc);
        }
        let ext = self.sequence.observe(frame.seq);
        let hash = payload_hash(&frame.pcm);
        if let Some(previous) = self.seen.get(&ext) {
            if *previous == hash {
                self.counts.duplicate += 1;
                return InsertOutcome::Duplicate;
            }
            self.counts.conflict += 1;
            return InsertOutcome::Conflict;
        }
        self.seen.insert(ext, hash);
        if self.floor.is_some_and(|floor| ext < floor) {
            self.counts.late += 1;
            return InsertOutcome::Late;
        }
        if let Some(high) = self.sequence.high_ext() {
            if high.saturating_sub(ext) > self.capacity as u64 {
                self.counts.late += 1;
                return InsertOutcome::Late;
            }
            if ext.saturating_sub(high) > self.capacity as u64 {
                self.counts.late += 1;
                return InsertOutcome::Late;
            }
        }
        self.frames.insert(ext, frame);
        let mut overrun = None;
        while self.frames.len() > self.capacity {
            if let Some((evicted, _)) = self.frames.pop_first() {
                self.counts.overrun += 1;
                overrun = Some(evicted);
            }
        }
        if let Some(evicted_seq) = overrun {
            InsertOutcome::Overrun {
                evicted_seq,
                buffered: self.frames.len(),
            }
        } else {
            InsertOutcome::Inserted {
                seq: ext,
                buffered: self.frames.len(),
            }
        }
    }

    pub fn take(&mut self, ext_seq: u64) -> Option<Frame> {
        self.frames.remove(&ext_seq)
    }

    pub fn earliest_ext(&self) -> Option<u64> {
        self.frames.keys().next().copied()
    }

    pub fn peek_earliest(&self) -> Option<&Frame> {
        self.frames.values().next()
    }

    pub fn len(&self) -> usize {
        self.frames.len()
    }

    pub fn is_empty(&self) -> bool {
        self.frames.is_empty()
    }

    pub fn counts(&self) -> &ReorderCounts {
        &self.counts
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::FRAME_SAMPLES;

    fn frame(seq: u16, sample: i16) -> Frame {
        Frame {
            ssrc: 9,
            seq,
            ts: seq as u64 * 960,
            arrival_us: seq as u64 * 20_000,
            pcm: vec![sample; FRAME_SAMPLES],
        }
    }

    #[test]
    fn orders_and_deduplicates_packets() {
        let mut buffer = ReorderBuffer::new(8);
        assert!(matches!(
            buffer.insert(frame(2, 2)),
            InsertOutcome::Inserted { .. }
        ));
        assert!(matches!(
            buffer.insert(frame(1, 1)),
            InsertOutcome::Inserted { .. }
        ));
        assert_eq!(buffer.earliest_ext(), Some(1));
        assert_eq!(buffer.insert(frame(2, 2)), InsertOutcome::Duplicate);
        assert_eq!(buffer.counts().duplicate, 1);
    }

    #[test]
    fn first_payload_wins_conflict() {
        let mut buffer = ReorderBuffer::new(8);
        buffer.insert(frame(1, 1));
        assert_eq!(buffer.insert(frame(1, 2)), InsertOutcome::Conflict);
        let kept = buffer.take(1).unwrap();
        assert_eq!(kept.pcm[0], 1);
        assert_eq!(buffer.counts().conflict, 1);
    }

    #[test]
    fn floor_rejects_already_played_sequence() {
        let mut buffer = ReorderBuffer::new(8);
        buffer.insert(frame(1, 1));
        buffer.set_floor(2);
        assert!(matches!(
            buffer.insert(frame(2, 2)),
            InsertOutcome::Inserted { .. }
        ));
        assert_eq!(buffer.insert(frame(1, 1)), InsertOutcome::Duplicate);
        assert_eq!(buffer.insert(frame(0, 0)), InsertOutcome::Late);
    }

    #[test]
    fn full_window_evicts_oldest() {
        let mut buffer = ReorderBuffer::new(2);
        buffer.insert(frame(1, 1));
        buffer.insert(frame(2, 2));
        match buffer.insert(frame(3, 3)) {
            InsertOutcome::Overrun { evicted_seq, .. } => assert_eq!(evicted_seq, 1),
            other => panic!("unexpected {other:?}"),
        }
        assert_eq!(buffer.counts().overrun, 1);
        assert_eq!(buffer.len(), 2);
    }
}

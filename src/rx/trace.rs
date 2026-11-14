use crate::model::{pcm_from_hex, Frame, Packet, FRAME_MS, SAMPLE_RATE};
use serde::{Deserialize, Serialize};
use std::fs::File;
use std::io::{BufRead, BufReader};
use std::path::Path;
use thiserror::Error;

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct TraceHeader {
    pub trace_version: u32,
    pub frames_total: u64,
    pub duration_ms: u64,
    pub sample_rate: u32,
    pub frame_ms: u64,
    pub packet_count: u64,
    pub seed: u64,
}

#[derive(Debug, Deserialize)]
struct TraceLine {
    arrival_ms: u64,
    ssrc: u32,
    seq: u16,
    ts: u64,
    pcm: String,
}

pub struct TraceReader {
    reader: BufReader<File>,
    header: TraceHeader,
    previous_arrival_ms: Option<u64>,
    records: u64,
    next_line_number: u64,
    finished: bool,
}

impl TraceReader {
    pub fn open(path: &Path) -> Result<(TraceHeader, Self), TraceError> {
        let file = File::open(path).map_err(TraceError::Io)?;
        let mut reader = BufReader::new(file);
        let mut next_line_number = 0;
        let (line_number, line) = read_next_nonempty(&mut reader, &mut next_line_number)?
            .ok_or(TraceError::MissingHeader)?;
        let header: TraceHeader =
            serde_json::from_str(&line).map_err(|source| TraceError::Json {
                line: line_number,
                source,
            })?;
        validate_header(&header)?;
        Ok((
            header.clone(),
            Self {
                reader,
                header,
                previous_arrival_ms: None,
                records: 0,
                next_line_number,
                finished: false,
            },
        ))
    }

    pub fn next_frame(&mut self) -> Option<Result<Frame, TraceError>> {
        if self.finished {
            return None;
        }
        let next = match read_next_nonempty(&mut self.reader, &mut self.next_line_number) {
            Ok(value) => value,
            Err(error) => {
                self.finished = true;
                return Some(Err(error));
            }
        };
        let Some((line_number, line)) = next else {
            self.finished = true;
            return None;
        };
        let record: TraceLine = match serde_json::from_str(&line) {
            Ok(record) => record,
            Err(source) => {
                self.finished = true;
                return Some(Err(TraceError::Json {
                    line: line_number,
                    source,
                }));
            }
        };
        if let Some(previous) = self.previous_arrival_ms {
            if record.arrival_ms < previous {
                self.finished = true;
                return Some(Err(TraceError::TimeRollback {
                    line: line_number,
                    previous,
                    current: record.arrival_ms,
                }));
            }
        }
        let pcm = match pcm_from_hex(&record.pcm) {
            Ok(pcm) => pcm,
            Err(source) => {
                self.finished = true;
                return Some(Err(TraceError::Packet {
                    line: line_number,
                    source,
                }));
            }
        };
        let packet = Packet {
            ssrc: record.ssrc,
            seq: record.seq,
            ts: record.ts,
            arrival_us: record.arrival_ms.saturating_mul(1_000),
            pcm,
        };
        let frame = match Frame::from_packet(packet) {
            Ok(frame) => frame,
            Err(source) => {
                self.finished = true;
                return Some(Err(TraceError::Packet {
                    line: line_number,
                    source,
                }));
            }
        };
        self.previous_arrival_ms = Some(record.arrival_ms);
        self.records += 1;
        Some(Ok(frame))
    }

    pub fn finish(&mut self) -> Result<(), TraceError> {
        if self.records != self.header.packet_count {
            return Err(TraceError::RecordCount {
                expected: self.header.packet_count,
                actual: self.records,
            });
        }
        Ok(())
    }
}

#[derive(Default)]
pub struct VirtualClock {
    now_ms: u64,
}

impl VirtualClock {
    pub fn advance_to(&mut self, arrival_ms: u64) -> Result<u64, TraceError> {
        if arrival_ms < self.now_ms {
            return Err(TraceError::TimeRollback {
                line: 0,
                previous: self.now_ms,
                current: arrival_ms,
            });
        }
        self.now_ms = arrival_ms;
        Ok(self.now_ms)
    }
}

fn validate_header(header: &TraceHeader) -> Result<(), TraceError> {
    if header.trace_version != 1 {
        return Err(TraceError::Header(format!(
            "unsupported trace_version {}",
            header.trace_version
        )));
    }
    if header.sample_rate != SAMPLE_RATE || header.frame_ms != FRAME_MS {
        return Err(TraceError::Header(format!(
            "expected {} Hz / {} ms frames",
            SAMPLE_RATE, FRAME_MS
        )));
    }
    if header.frames_total == 0 || header.duration_ms == 0 {
        return Err(TraceError::Header("empty trace header".into()));
    }
    Ok(())
}

fn read_next_nonempty(
    reader: &mut BufReader<File>,
    line_number: &mut u64,
) -> Result<Option<(u64, String)>, TraceError> {
    let mut line = String::new();
    loop {
        *line_number = line_number.saturating_add(1);
        line.clear();
        let read = reader.read_line(&mut line).map_err(TraceError::Io)?;
        if read == 0 {
            return Ok(None);
        }
        let trimmed = line.trim();
        if !trimmed.is_empty() {
            return Ok(Some((*line_number, trimmed.to_string())));
        }
    }
}

#[derive(Debug, Error)]
pub enum TraceError {
    #[error("I/O error: {0}")]
    Io(#[from] std::io::Error),
    #[error("trace has no header")]
    MissingHeader,
    #[error("invalid trace header: {0}")]
    Header(String),
    #[error("trace JSON error at line {line}: {source}")]
    Json {
        line: u64,
        #[source]
        source: serde_json::Error,
    },
    #[error("trace timestamp rollback at line {line}: {previous} -> {current} ms")]
    TimeRollback {
        line: u64,
        previous: u64,
        current: u64,
    },
    #[error("trace packet error at line {line}: {source}")]
    Packet {
        line: u64,
        #[source]
        source: crate::model::ModelError,
    },
    #[error("truncated trace: expected {expected} records, read {actual}")]
    RecordCount { expected: u64, actual: u64 },
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::{pcm_to_hex, FRAME_SAMPLES};

    fn write_fixture(name: &str, body: String) -> std::path::PathBuf {
        let path = std::env::temp_dir().join(name);
        std::fs::write(&path, body).unwrap();
        path
    }

    fn header(packet_count: u64) -> String {
        format!(
            concat!(
                "{{\"trace_version\":1,\"frames_total\":2,\"duration_ms\":40,",
                "\"sample_rate\":48000,\"frame_ms\":20,\"packet_count\":{},",
                "\"seed\":7}}\n"
            ),
            packet_count
        )
    }

    fn record(arrival_ms: u64, seq: u16) -> String {
        let pcm = pcm_to_hex(&vec![100; FRAME_SAMPLES]);
        format!(
            "{{\"arrival_ms\":{arrival_ms},\"ssrc\":1,\"seq\":{seq},\"ts\":{},\"pcm\":\"{pcm}\"}}\n",
            seq as u64 * 960
        )
    }

    #[test]
    fn reads_valid_trace() {
        let path = write_fixture(
            "valid_trace.jsonl",
            header(2) + &record(0, 0) + &record(20, 1),
        );
        let (parsed_header, mut reader) = TraceReader::open(&path).unwrap();
        assert_eq!(parsed_header.frames_total, 2);
        assert!(reader.next_frame().unwrap().is_ok());
        assert!(reader.next_frame().unwrap().is_ok());
        assert!(reader.next_frame().is_none());
        reader.finish().unwrap();
        let _ = std::fs::remove_file(path);
    }

    #[test]
    fn rejects_timestamp_rollback() {
        let path = write_fixture(
            "rollback_trace.jsonl",
            header(2) + &record(20, 0) + &record(10, 1),
        );
        let (_, mut reader) = TraceReader::open(&path).unwrap();
        assert!(reader.next_frame().unwrap().is_ok());
        assert!(matches!(
            reader.next_frame().unwrap(),
            Err(TraceError::TimeRollback { .. })
        ));
        let _ = std::fs::remove_file(path);
    }

    #[test]
    fn rejects_truncated_trace() {
        let path = write_fixture("truncated_trace.jsonl", header(2) + &record(0, 0));
        let (_, mut reader) = TraceReader::open(&path).unwrap();
        assert!(reader.next_frame().unwrap().is_ok());
        assert!(reader.next_frame().is_none());
        assert!(matches!(
            reader.finish(),
            Err(TraceError::RecordCount { .. })
        ));
        let _ = std::fs::remove_file(path);
    }

    #[test]
    fn rejects_missing_field() {
        let path = write_fixture("missing_trace.jsonl", header(1) + "{\"arrival_ms\":0}\n");
        let (_, mut reader) = TraceReader::open(&path).unwrap();
        assert!(matches!(
            reader.next_frame().unwrap(),
            Err(TraceError::Json { .. })
        ));
        let _ = std::fs::remove_file(path);
    }
}

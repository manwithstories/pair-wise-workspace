use std::fmt;

pub mod dhcp;
pub mod edr;
pub mod lexer;
pub mod nat;

pub use lexer::{scan_line, stable_digest, summarize};

#[derive(Debug, Clone)]
pub struct Common {
    pub device: String,
    pub batch_seq: u32,
    pub event_seq: u64,
    pub ts: i64,
    pub bits: u8,
    pub anchor: Option<i64>,
    pub content_hash: String,
}

#[derive(Debug, Clone)]
pub struct Lease {
    pub common: Common,
    pub ip: String,
    pub host: String,
    pub mac: String,
    pub lease_start: i64,
    pub lease_end: i64,
    pub event: String,
    pub is_new: bool,
}

#[derive(Debug, Clone)]
pub struct NatFlow {
    pub common: Common,
    pub src_ip: Option<String>,
    pub src_port: Option<u16>,
    pub public_ip: String,
    pub public_port: u16,
    pub dst_ip: String,
    pub dst_port: u16,
    pub protocol: String,
    pub bytes: u64,
    pub direction: String,
    pub action: String,
    pub signature: Option<String>,
}

#[derive(Debug, Clone)]
pub struct HostEvent {
    pub common: Common,
    pub host_id: String,
    pub ip: Option<String>,
    pub action: String,
    pub src_ip: Option<String>,
    pub dst_ip: Option<String>,
    pub dst_port: Option<u16>,
    pub user: Option<String>,
    pub signature: Option<String>,
    pub severity: u8,
}

#[derive(Debug, Clone)]
pub enum ParsedRecord {
    Lease(Lease),
    NatFlow(NatFlow),
    HostEvent(HostEvent),
}

impl ParsedRecord {
    pub fn common(&self) -> &Common {
        match self {
            Self::Lease(record) => &record.common,
            Self::NatFlow(record) => &record.common,
            Self::HostEvent(record) => &record.common,
        }
    }

    pub fn kind(&self) -> &'static str {
        match self {
            Self::Lease(_) => "lease",
            Self::NatFlow(_) => "nat",
            Self::HostEvent(_) => "edr",
        }
    }

    pub fn fingerprint(&self) -> String {
        let common = self.common();
        format!(
            "{}:{}:{}:{}",
            common.device, common.batch_seq, common.ts, common.content_hash
        )
    }
}

#[derive(Debug, Clone)]
pub struct ParseReject {
    pub source: String,
    pub line_no: u64,
    pub summary: String,
    pub reason: String,
    pub fingerprint: String,
}

#[derive(Debug, Clone)]
pub enum ParseOutcome {
    Accepted(ParsedRecord),
    Rejected(ParseReject),
}

pub fn parse_line(kind: &str, line: &str, source: &str, line_no: u64) -> ParseOutcome {
    match kind {
        "dhcp" => dhcp::parse(line, source, line_no),
        "nat" => nat::parse(line, source, line_no),
        "edr" => edr::parse(line, source, line_no),
        _ => rejected(source, line_no, line, format!("unknown source kind {kind}")),
    }
}

pub(crate) fn reject_from_lex(
    source: &str,
    line_no: u64,
    line: &str,
    error: lexer::LexError,
) -> ParseOutcome {
    rejected(source, line_no, line, error.reason)
}

pub(crate) fn rejected(source: &str, line_no: u64, line: &str, reason: String) -> ParseOutcome {
    let summary = summarize(line, 120);
    let digest = stable_digest(line.as_bytes());
    ParseOutcome::Rejected(ParseReject {
        fingerprint: format!("reject:{source}:{line_no}:{digest}"),
        source: source.to_owned(),
        line_no,
        summary,
        reason,
    })
}

pub(crate) fn common_fields(fields: &lexer::ScannedLine, line: &str) -> Result<Common, String> {
    let device = fields.require("dev")?.to_owned();
    let batch_seq = parse_u32(fields, "batch")?;
    let event_seq = parse_u64(fields, "seq")?;
    let ts = parse_i64(fields, "ts")?;
    let bits = match fields.get("bits") {
        Some("32") => 32,
        Some("64") | None => 64,
        Some(value) => return Err(format!("unsupported clock width {value}")),
    };
    let anchor = match fields.get("anchor") {
        Some(value) => Some(parse_i64_value(value, "anchor")?),
        None => None,
    };
    if bits == 32 && anchor.is_none() {
        return Err("32-bit clock requires an anchor field".to_owned());
    }
    Ok(Common {
        device,
        batch_seq,
        event_seq,
        ts,
        bits,
        anchor,
        content_hash: stable_digest(line.as_bytes()),
    })
}

pub(crate) fn parse_i64(fields: &lexer::ScannedLine, key: &str) -> Result<i64, String> {
    parse_i64_value(fields.require(key)?, key)
}

pub(crate) fn parse_i64_value(value: &str, key: &str) -> Result<i64, String> {
    value
        .parse::<i64>()
        .map_err(|_| format!("field {key} is not an i64: {value:?}"))
}

pub(crate) fn parse_u64(fields: &lexer::ScannedLine, key: &str) -> Result<u64, String> {
    fields
        .require(key)?
        .parse::<u64>()
        .map_err(|_| format!("field {key} is not a u64"))
}

pub(crate) fn parse_u32(fields: &lexer::ScannedLine, key: &str) -> Result<u32, String> {
    fields
        .require(key)?
        .parse::<u32>()
        .map_err(|_| format!("field {key} is not a u32"))
}

pub(crate) fn parse_u16(fields: &lexer::ScannedLine, key: &str) -> Result<u16, String> {
    fields
        .require(key)?
        .parse::<u16>()
        .map_err(|_| format!("field {key} is not a u16"))
}

pub(crate) fn parse_optional_u16(
    fields: &lexer::ScannedLine,
    key: &str,
) -> Result<Option<u16>, String> {
    match fields.get(key) {
        None | Some("-") | Some("") => Ok(None),
        Some(value) => value
            .parse::<u16>()
            .map(Some)
            .map_err(|_| format!("field {key} is not a u16")),
    }
}

pub(crate) fn validate_ipv4(value: &str, key: &str) -> Result<(), String> {
    let parts: Vec<&str> = value.split('.').collect();
    if parts.len() != 4 {
        return Err(format!("field {key} is not IPv4: {value:?}"));
    }
    for part in parts {
        if part.is_empty() || part.len() > 3 {
            return Err(format!("field {key} is not IPv4: {value:?}"));
        }
        let octet = part
            .parse::<u16>()
            .map_err(|_| format!("field {key} is not IPv4: {value:?}"))?;
        if octet > 255 {
            return Err(format!("field {key} is not IPv4: {value:?}"));
        }
    }
    Ok(())
}

impl fmt::Display for ParsedRecord {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}:{}", self.kind(), self.common().device)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn malformed_common_fields_are_rejected() {
        let outcome = parse_line(
            "edr",
            "ts=nope dev=edr batch=1 seq=1 host_id=h action=x",
            "edr.log",
            1,
        );
        match outcome {
            ParseOutcome::Rejected(reject) => assert!(reject.reason.contains("ts")),
            ParseOutcome::Accepted(_) => panic!("expected rejection"),
        }
    }
}

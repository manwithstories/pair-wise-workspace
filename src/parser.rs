//! Streaming, auto-detecting line parser.
//!
//! A single entry point ([`parse_line`]) is shared by live ingestion and
//! replay. Bad lines never abort the run; they are returned as
//! [`crate::model::Quarantined`] entries and persisted to the quarantine table.

use crate::error::Result;
use crate::model::{Quarantined, Record};

/// FNV-1a 64 bit content fingerprint, rendered hex. Deterministic and free of
/// platform hasher randomisation, so re-importing the same batch is stable.
pub fn fingerprint(source_file: &str, line_no: usize, raw: &str) -> String {
    let mut hash: u64 = 0xcbf29ce484222325;
    let mut feed = |byte: u8| {
        hash ^= byte as u64;
        hash = hash.wrapping_mul(0x100000001b3);
    };
    for b in source_file.as_bytes() {
        feed(*b);
    }
    feed(b'|');
    for b in line_no.to_le_bytes() {
        feed(b);
    }
    feed(b'|');
    for b in raw.as_bytes() {
        feed(*b);
    }
    format!("{:016x}", hash)
}

enum Kind {
    Nginx,
    Sshd,
    Zeek,
}

fn detect(line: &str) -> Option<Kind> {
    if line.contains("sshd[") {
        Some(Kind::Sshd)
    } else if line.contains('\t') {
        Some(Kind::Zeek)
    } else if looks_like_nginx(line) {
        Some(Kind::Nginx)
    } else {
        None
    }
}

fn looks_like_nginx(line: &str) -> bool {
    let first = line.split_whitespace().next().unwrap_or("");
    first.contains('.')
        && first
            .chars()
            .next()
            .map(|c| c.is_ascii_digit())
            .unwrap_or(false)
}

/// Parse one raw line. `Ok(None)` means the line is blank and must be skipped;
/// `Err` is only returned for IO-independent structural problems, surfaced via
/// the quarantine channel by [`parse_file_line`] instead of unwinding.
pub fn parse_line(
    source_file: &str,
    line_no: usize,
    raw: &str,
) -> std::result::Result<Option<Record>, String> {
    let trimmed = raw.trim();
    if trimmed.is_empty() {
        return Ok(None);
    }
    let kind = detect(trimmed).ok_or_else(|| "unrecognised log format".to_string())?;
    let record = match kind {
        Kind::Nginx => parse_nginx(source_file, line_no, trimmed)?,
        Kind::Sshd => parse_sshd(source_file, line_no, trimmed)?,
        Kind::Zeek => parse_zeek(source_file, line_no, trimmed)?,
    };
    Ok(Some(record))
}

/// Convenience wrapper returning either a record or a quarantine entry.
pub fn parse_file_line(source_file: &str, line_no: usize, raw: &str) -> Result<Option<Either>> {
    match parse_line(source_file, line_no, raw) {
        Ok(Some(r)) => Ok(Some(Either::Record(r))),
        Ok(None) => Ok(None),
        Err(reason) => Ok(Some(Either::Quarantine(Quarantined {
            source_file: source_file.to_string(),
            line_no,
            raw: raw.to_string(),
            reason,
        }))),
    }
}

pub enum Either {
    Record(Record),
    Quarantine(Quarantined),
}

fn next_split<'a>(parts: &mut impl Iterator<Item = &'a str>) -> Option<&'a str> {
    parts
        .next()
        .map(str::trim)
        .filter(|s| !s.is_empty() && *s != "-")
}

fn parse_nginx(
    source_file: &str,
    line_no: usize,
    raw: &str,
) -> std::result::Result<Record, String> {
    // <ip> - - [<tick>] "<METHOD> <path> HTTP/x" <status> <bytes> "<ref>" "<ua>"
    let ip = raw
        .split_whitespace()
        .next()
        .ok_or("missing src_ip")?
        .to_string();
    let tick: u32 = extract_bracketed_u32(raw).ok_or("missing or bad tick")?;

    // Split on double quotes: [prefix, request, mid, referer, mid2, ua, suffix]
    let quoted: Vec<&str> = raw.split('"').collect();
    if quoted.len() < 6 {
        return Err("nginx row missing quoted segments".into());
    }
    let request = quoted[1];
    let mut req_parts = request.split_whitespace();
    let method = req_parts.next().map(str::to_string);
    let path = req_parts.next().map(str::to_string);
    let user_agent = Some(quoted[5].trim().to_string()).filter(|s| !s.is_empty());

    // quoted[2] sits between request and referer: " <status> <bytes> "
    let mut num_it = quoted[2].split_whitespace();
    let status = num_it.next().and_then(|s| s.parse().ok());
    let bytes = num_it.next().and_then(|s| s.parse().ok());

    if ip.is_empty() {
        return Err("missing src_ip".into());
    }
    Ok(Record {
        fingerprint: fingerprint(source_file, line_no, raw),
        source_file: source_file.to_string(),
        line_no,
        raw_tick: tick,
        event: "http".to_string(),
        src_ip: ip,
        user: None,
        user_agent,
        dest_ip: None,
        dest_port: None,
        status,
        method,
        path,
        bytes,
    })
}

fn parse_sshd(source_file: &str, line_no: usize, raw: &str) -> std::result::Result<Record, String> {
    // <tick> host sshd[pid]: <message>  for user <u> from <ip> ...
    let tick: u32 = raw
        .split_whitespace()
        .next()
        .and_then(|s| s.parse().ok())
        .ok_or("missing or bad tick")?;
    let (event, status) = if raw.contains("Failed password") {
        ("login_failure".to_string(), Some(401u16))
    } else if raw.contains("Accepted password") || raw.contains("Accepted publickey") {
        ("login_success".to_string(), Some(200u16))
    } else if raw.contains("session closed") || raw.contains("Disconnected") {
        ("logout".to_string(), Some(204u16))
    } else if raw.contains("Invalid user") {
        ("login_failure".to_string(), Some(401u16))
    } else {
        ("sshd_other".to_string(), None)
    };
    let user = extract_after(raw, "for user")
        .or_else(|| extract_after(raw, "for invalid user"))
        .or_else(|| extract_after(raw, "user"));
    let src_ip = extract_after(raw, "from")
        .map(|s| s.split_whitespace().next().unwrap_or("").to_string())
        .filter(|s| !s.is_empty())
        .ok_or("missing src_ip")?;
    Ok(Record {
        fingerprint: fingerprint(source_file, line_no, raw),
        source_file: source_file.to_string(),
        line_no,
        raw_tick: tick,
        event,
        src_ip,
        user,
        user_agent: Some("ssh".to_string()),
        dest_ip: None,
        dest_port: Some(22),
        status,
        method: None,
        path: None,
        bytes: None,
    })
}

fn parse_zeek(source_file: &str, line_no: usize, raw: &str) -> std::result::Result<Record, String> {
    // <tick>\t<uid>\t<src_ip>\t<src_port>\t<dest_ip>\t<dest_port>\t<proto>\t<status>\t<bytes>\t<ua>
    let fields: Vec<&str> = raw.split('\t').collect();
    if fields.len() < 9 {
        return Err("zeek row has too few fields".into());
    }
    let mut it = fields.iter().copied();
    let tick: u32 = next_split(&mut it)
        .and_then(|s| s.parse().ok())
        .ok_or("missing or bad tick")?;
    let _uid = next_split(&mut it);
    let src_ip = next_split(&mut it).ok_or("missing src_ip")?.to_string();
    let _src_port = next_split(&mut it);
    let dest_ip = next_split(&mut it).map(str::to_string);
    let dest_port = next_split(&mut it).and_then(|s| s.parse().ok());
    let _proto = next_split(&mut it);
    let status = next_split(&mut it).and_then(|s| s.parse().ok());
    let bytes = next_split(&mut it).and_then(|s| s.parse().ok());
    let user_agent = next_split(&mut it).map(str::to_string);
    Ok(Record {
        fingerprint: fingerprint(source_file, line_no, raw),
        source_file: source_file.to_string(),
        line_no,
        raw_tick: tick,
        event: "conn".to_string(),
        src_ip,
        user: None,
        user_agent,
        dest_ip,
        dest_port,
        status,
        method: None,
        path: None,
        bytes,
    })
}

fn extract_bracketed_u32(line: &str) -> Option<u32> {
    let open = line.find('[')?;
    let close = line[open..].find(']')? + open;
    line[open + 1..close].trim().parse().ok()
}

fn extract_after(line: &str, marker: &str) -> Option<String> {
    let pos = line.find(marker)?;
    let rest = line[pos + marker.len()..].trim();
    let token = rest.split_whitespace().next()?;
    Some(token.to_string())
}

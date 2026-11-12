//! Canonical record model shared by every pipeline stage.
//!
//! All three native log formats (nginx / sshd / zeek) are normalised into the
//! same [`Record`] so downstream stages are format agnostic.

use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Record {
    /// Stable content fingerprint; conflicts are counted, never overwritten.
    pub fingerprint: String,
    pub source_file: String,
    pub line_no: usize,

    /// Raw 32-bit device tick as printed on the wire.
    pub raw_tick: u32,
    /// Normalised event name, e.g. `http`, `login_success`, `conn`.
    pub event: String,

    pub src_ip: String,
    pub user: Option<String>,
    pub user_agent: Option<String>,
    pub dest_ip: Option<String>,
    pub dest_port: Option<u16>,
    pub status: Option<u16>,
    pub method: Option<String>,
    pub path: Option<String>,
    pub bytes: Option<u64>,
}

impl Record {
    /// Value of a declarative grouping field.
    pub fn group_value(&self, field: &str) -> String {
        match field {
            "src_ip" => self.src_ip.clone(),
            "user" => self.user.clone().unwrap_or_default(),
            "user_agent" => self.user_agent.clone().unwrap_or_default(),
            "dest_ip" => self.dest_ip.clone().unwrap_or_default(),
            "dest_port" => self.dest_port.map(|p| p.to_string()).unwrap_or_default(),
            "status" => self.status.map(|s| s.to_string()).unwrap_or_default(),
            "event" => self.event.clone(),
            _ => String::new(),
        }
    }

    pub fn is_4xx(&self) -> bool {
        matches!(self.status, Some(s) if (400..500).contains(&s))
    }
}

/// One quarantined raw line plus the reason it could not be parsed.
#[derive(Debug, Clone)]
pub struct Quarantined {
    pub source_file: String,
    pub line_no: usize,
    pub raw: String,
    pub reason: String,
}

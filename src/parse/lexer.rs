use std::collections::HashMap;

#[derive(Debug, Clone)]
pub struct ScannedLine {
    pub fields: HashMap<String, String>,
    pub summary: String,
}

impl ScannedLine {
    pub fn get(&self, key: &str) -> Option<&str> {
        self.fields.get(key).map(String::as_str)
    }

    pub fn require(&self, key: &str) -> Result<&str, String> {
        self.get(key)
            .filter(|value| !value.is_empty())
            .ok_or_else(|| format!("missing field {key}"))
    }

    pub fn optional_nonempty(&self, key: &str) -> Option<String> {
        self.get(key)
            .filter(|value| !value.is_empty() && *value != "-")
            .map(ToOwned::to_owned)
    }
}

#[derive(Debug, Clone, thiserror::Error)]
#[error("{reason}")]
pub struct LexError {
    pub reason: String,
}

/// Scans a line into lower-cased key/value fields.
///
/// The scanner is byte-oriented and supports whitespace/comma separated
/// `key=value` pairs, quoted values, and common backslash escapes. It never
/// panics on malformed appliance output.
pub fn scan_line(line: &str) -> Result<ScannedLine, LexError> {
    let summary = summarize(line, 120);
    let bytes = line.as_bytes();
    let mut fields = HashMap::new();
    let mut i = 0usize;

    if bytes.starts_with(&[0xef, 0xbb, 0xbf]) {
        i = 3;
    }

    while i < bytes.len() {
        while i < bytes.len() && is_separator(bytes[i]) {
            i += 1;
        }
        if i >= bytes.len() {
            break;
        }
        if bytes[i] == b'#' {
            break;
        }

        let key_start = i;
        while i < bytes.len() && bytes[i] != b'=' && !is_separator(bytes[i]) {
            i += 1;
        }
        if i >= bytes.len() || bytes[i] != b'=' {
            return Err(LexError {
                reason: format!("token at byte {key_start} is not key=value"),
            });
        }

        let key = &line[key_start..i];
        validate_key(key)?;
        let key = key.to_ascii_lowercase();
        i += 1;

        if i >= bytes.len() {
            return Err(LexError {
                reason: format!("field {key} has no value"),
            });
        }

        let value;
        if bytes[i] == b'"' {
            i += 1;
            let mut decoded = String::new();
            let mut closed = false;
            while i < bytes.len() {
                match bytes[i] {
                    b'"' => {
                        i += 1;
                        closed = true;
                        break;
                    }
                    b'\\' => {
                        i += 1;
                        if i >= bytes.len() {
                            return Err(LexError {
                                reason: format!("field {key} ends in an escape"),
                            });
                        }
                        match bytes[i] {
                            b'n' => decoded.push('\n'),
                            b'r' => decoded.push('\r'),
                            b't' => decoded.push('\t'),
                            b'\\' => decoded.push('\\'),
                            b'"' => decoded.push('"'),
                            other => {
                                return Err(LexError {
                                    reason: format!(
                                        "field {key} has unsupported escape \\\\{}",
                                        other as char
                                    ),
                                });
                            }
                        }
                        i += 1;
                    }
                    _ => {
                        let ch = line[i..].chars().next().expect("valid utf-8 boundary");
                        decoded.push(ch);
                        i += ch.len_utf8();
                    }
                }
            }
            if !closed {
                return Err(LexError {
                    reason: format!("field {key} has an unterminated quote"),
                });
            }
            if i < bytes.len() && !is_separator(bytes[i]) {
                return Err(LexError {
                    reason: format!("field {key} has trailing bytes after quote"),
                });
            }
            value = decoded;
        } else {
            let value_start = i;
            while i < bytes.len() && !is_separator(bytes[i]) {
                i += 1;
            }
            value = line[value_start..i].to_owned();
        }

        if value.is_empty() {
            return Err(LexError {
                reason: format!("field {key} has an empty value"),
            });
        }
        if fields.insert(key.clone(), value).is_some() {
            return Err(LexError {
                reason: format!("duplicate field {key}"),
            });
        }
    }

    if fields.is_empty() {
        return Err(LexError {
            reason: "line contains no key=value fields".to_owned(),
        });
    }
    Ok(ScannedLine { fields, summary })
}

fn is_separator(byte: u8) -> bool {
    matches!(byte, b' ' | b'\t' | b'\r' | b'\n' | b',')
}

fn validate_key(key: &str) -> Result<(), LexError> {
    if key.is_empty() {
        return Err(LexError {
            reason: "empty field name".to_owned(),
        });
    }
    if !key
        .bytes()
        .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-' | b'.'))
    {
        return Err(LexError {
            reason: format!("invalid field name {key:?}"),
        });
    }
    Ok(())
}

pub fn summarize(line: &str, max_bytes: usize) -> String {
    if line.len() <= max_bytes {
        return line.to_owned();
    }
    let mut end = max_bytes;
    while end > 0 && !line.is_char_boundary(end) {
        end -= 1;
    }
    line[..end].to_owned()
}

pub fn stable_digest(bytes: &[u8]) -> String {
    let first = fnv1a(bytes, 0xcbf29ce484222325);
    let second = fnv1a(bytes, 0x84222325cbf29ce4);
    format!("{first:016x}{second:016x}")
}

fn fnv1a(bytes: &[u8], seed: u64) -> u64 {
    let mut hash = seed;
    for byte in bytes {
        hash ^= u64::from(*byte);
        hash = hash.wrapping_mul(0x100000001b3);
    }
    hash
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_quoted_and_unquoted_fields() {
        let line = r#"ts=10 host="finance 01" note="x y""#;
        let parsed = scan_line(line).unwrap();
        assert_eq!(parsed.get("host"), Some("finance 01"));
    }

    #[test]
    fn rejects_unterminated_quote() {
        let error = scan_line(r#"host="unterminated"#).unwrap_err();
        assert!(error.reason.contains("unterminated"));
    }

    #[test]
    fn rejects_duplicate_key() {
        let error = scan_line("a=1 a=2").unwrap_err();
        assert!(error.reason.contains("duplicate"));
    }

    #[test]
    fn summaries_are_truncated_to_120_bytes_without_splitting_utf8() {
        let line = format!("{}密{}", "a".repeat(115), "b".repeat(20));
        let summary = summarize(&line, 120);
        assert!(summary.len() <= 120);
        assert!(line.is_char_boundary(summary.len()));
    }
}

use super::*;

pub fn parse(line: &str, source: &str, line_no: u64) -> ParseOutcome {
    let fields = match scan_line(line) {
        Ok(fields) => fields,
        Err(error) => return reject_from_lex(source, line_no, line, error),
    };
    let common = match common_fields(&fields, line) {
        Ok(common) => common,
        Err(reason) => return rejected(source, line_no, line, reason),
    };

    let parsed = (|| -> Result<Lease, String> {
        let ip = fields.optional_nonempty("ip").ok_or("missing field ip")?;
        validate_ipv4(&ip, "ip")?;
        let host = fields.require("host")?.to_owned();
        let mac = fields.require("mac")?.to_owned();
        let lease_start = parse_i64(&fields, "lease_start")?;
        let lease_end = parse_i64(&fields, "lease_end")?;
        if lease_end <= lease_start {
            return Err("lease_end must be greater than lease_start".to_owned());
        }
        let event = fields.get("event").unwrap_or("new").to_ascii_lowercase();
        if !matches!(event.as_str(), "new" | "renew" | "bound" | "change") {
            return Err(format!("unsupported lease event {event:?}"));
        }
        let is_new = matches!(event.as_str(), "new" | "bound" | "change");
        Ok(Lease {
            common,
            ip,
            host,
            mac,
            lease_start,
            lease_end,
            event,
            is_new,
        })
    })();

    match parsed {
        Ok(record) => ParseOutcome::Accepted(ParsedRecord::Lease(record)),
        Err(reason) => rejected(source, line_no, line, reason),
    }
}

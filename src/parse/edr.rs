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

    let parsed = (|| -> Result<HostEvent, String> {
        let host_id = fields.require("host_id")?.to_owned();
        let ip = fields.optional_nonempty("ip");
        if let Some(ip) = &ip {
            validate_ipv4(ip, "ip")?;
        }
        let action = fields.require("action")?.to_ascii_lowercase();
        let src_ip = fields.optional_nonempty("src_ip");
        if let Some(ip) = &src_ip {
            validate_ipv4(ip, "src_ip")?;
        }
        let dst_ip = fields.optional_nonempty("dst_ip");
        if let Some(ip) = &dst_ip {
            validate_ipv4(ip, "dst_ip")?;
        }
        let dst_port = parse_optional_u16(&fields, "dst_port")?;
        let user = fields.optional_nonempty("user");
        let signature = fields.optional_nonempty("signature");
        let severity = match fields.get("severity") {
            Some(value) => value
                .parse::<u8>()
                .map_err(|_| "field severity is not a u8".to_owned())?,
            None => 1,
        };
        Ok(HostEvent {
            common,
            host_id,
            ip,
            action,
            src_ip,
            dst_ip,
            dst_port,
            user,
            signature,
            severity,
        })
    })();

    match parsed {
        Ok(record) => ParseOutcome::Accepted(ParsedRecord::HostEvent(record)),
        Err(reason) => rejected(source, line_no, line, reason),
    }
}

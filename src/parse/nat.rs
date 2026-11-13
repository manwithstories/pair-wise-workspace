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

    let parsed = (|| -> Result<NatFlow, String> {
        let src_ip = fields.optional_nonempty("src_ip");
        if let Some(ip) = &src_ip {
            validate_ipv4(ip, "src_ip")?;
        }
        let src_port = parse_optional_u16(&fields, "src_port")?;
        let public_ip = fields
            .optional_nonempty("pub_ip")
            .ok_or("missing field pub_ip")?;
        validate_ipv4(&public_ip, "pub_ip")?;
        let public_port = parse_u16(&fields, "pub_port")?;
        let dst_ip = fields
            .optional_nonempty("dst_ip")
            .ok_or("missing field dst_ip")?;
        validate_ipv4(&dst_ip, "dst_ip")?;
        let dst_port = parse_u16(&fields, "dst_port")?;
        let protocol = fields.get("proto").unwrap_or("tcp").to_ascii_lowercase();
        let bytes = parse_u64(&fields, "bytes")?;
        let direction = fields
            .get("direction")
            .unwrap_or("outbound")
            .to_ascii_lowercase();
        if !matches!(direction.as_str(), "outbound" | "inbound") {
            return Err(format!("unsupported direction {direction:?}"));
        }
        let action = fields.get("action").unwrap_or("open").to_ascii_lowercase();
        let signature = fields.optional_nonempty("signature");
        Ok(NatFlow {
            common,
            src_ip,
            src_port,
            public_ip,
            public_port,
            dst_ip,
            dst_port,
            protocol,
            bytes,
            direction,
            action,
            signature,
        })
    })();

    match parsed {
        Ok(record) => ParseOutcome::Accepted(ParsedRecord::NatFlow(record)),
        Err(reason) => rejected(source, line_no, line, reason),
    }
}

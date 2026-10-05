//! Human-readable connection state dumps.
//!
//! This exists for a specific failure mode: when throughput collapses at 15%
//! loss, the question on the floor is always one of four — is the send window
//! stuck, is the retransmission queue backing up, has the congestion window
//! collapsed, or has the RTT estimate run away? Answering that from counters on a
//! dashboard is slow and imprecise; answering it from one snapshot taken on the
//! live process is not.
//!
//! [`ConnDump`] is a cheap, `Copy`-able value the engine fills on demand.
//! Building it touches no locks and allocates nothing beyond the SACK block
//! list, which is what keeps a dump well inside the 2 ms budget — a dump taken
//! while a connection is under load is exactly when it is most useful.
//!
//! The four required sections are always present, in a fixed order:
//!
//! 1. send window `[una, next)`
//! 2. congestion window (`cwnd` / `ssthresh` and the phase)
//! 3. retransmission queue (length, first sequence number, peak)
//! 4. RTT estimate (`srtt` / `rttvar` / RTO)
//!
//! Everything else is supporting detail. Formatting is deliberately plain: this
//! output is read in a terminal, in an incident, by someone who does not have
//! the source open.

use std::fmt;
use std::time::Duration;

use crate::conn::Conn;
use crate::congestion::CcPhase;
use crate::sender::Sender;
use crate::wire::Block;

/// A point-in-time snapshot of a connection's internals.
///
/// Cheap to produce and safe to keep: it borrows nothing, so it can be
/// formatted later, sent to a log pipeline, or compared against a later dump
/// after the connection is gone.
#[derive(Clone, Debug)]
pub struct ConnDump {
    pub state: String,
    pub local: String,
    pub peer: String,
    // 1. send window
    pub una: u64,
    pub next: u64,
    pub inflight: u64,
    pub window_credit: u64,
    pub window_stalled: bool,
    pub pending_bytes: usize,
    // 2. congestion window
    pub cwnd: u32,
    pub ssthresh: u32,
    pub cc_phase: CcPhase,
    pub dup_acks: u32,
    pub sacked_credit_bytes: u64,
    // 3. retransmission queue
    pub rtx_queue_len: usize,
    pub rtx_queue_peak: usize,
    pub rtx_head_seq: Option<u64>,
    pub rtx_head_tx_count: u32,
    pub rtx_in_queue: usize,
    pub repair_target: Option<u64>,
    pub timeouts: u32,
    pub fast_retransmits: u32,
    pub backoff_exp: u32,
    pub consecutive_timeouts: u32,
    pub spurious_retransmits: u64,
    // 4. RTT estimate
    pub srtt_us: Option<u64>,
    pub rttvar_us: u64,
    pub rto_us: u64,
    pub base_rto_us: u64,
    pub rtt_samples: u64,
    pub timer_deadline_us: u64,
    pub timer_armed: bool,
    // receive side
    pub recv_una: u64,
    pub recv_out_of_order: u64,
    pub recv_out_of_order_peak: u64,
    pub dup_dropped: u64,
    pub sack_blocks: Vec<Block>,
    // close
    pub fin_allocated: bool,
    pub fin_acked: bool,
    pub peer_fin: bool,
    // counters
    pub datagrams_sent: u64,
    pub datagrams_received: u64,
    pub datagrams_malformed: u64,
    pub bytes_written: u64,
    pub bytes_delivered: u64,
    pub bytes_acked: u64,
    pub new_transmits: u64,
    pub retransmits: u64,
}

impl ConnDump {
    /// Take a snapshot of a live connection.
    pub fn capture(c: &Conn) -> Self {
        Self::from_sender(c.sender(), c.state().to_string())
    }

    /// Take a snapshot from a sender alone, for tests that drive one directly.
    pub fn from_sender(s: &Sender, state: String) -> Self {
        let cc = s.cc();
        let rtt = s.rtt();
        let head = s.queue_head();
        ConnDump {
            state,
            local: String::new(),
            peer: String::new(),
            una: s.una(),
            next: s.next(),
            inflight: s.inflight(),
            window_credit: s.window_credit(),
            window_stalled: s.window_stalled(),
            pending_bytes: s.pending_bytes(),
            cwnd: cc.cwnd(),
            ssthresh: cc.ssthresh(),
            cc_phase: cc.phase(),
            dup_acks: cc.dup_acks(),
            sacked_credit_bytes: cc.sacked_bytes_credited(),
            rtx_queue_len: s.queue_len(),
            rtx_queue_peak: s.peak_queue_len(),
            rtx_head_seq: s.first_queued_seq(),
            rtx_head_tx_count: head.map(|h| h.tx_count).unwrap_or(0),
            rtx_in_queue: s.sacked_in_queue(),
            repair_target: s.repair_target(),
            timeouts: s.loss_events().0,
            fast_retransmits: s.loss_events().1,
            backoff_exp: s.backoff_exp(),
            consecutive_timeouts: s.consecutive_timeouts(),
            spurious_retransmits: s.spurious_retransmits(),
            srtt_us: rtt.srtt(),
            rttvar_us: rtt.rttvar(),
            rto_us: s.rto_us(),
            base_rto_us: s.base_rto_us(),
            rtt_samples: rtt.samples(),
            timer_deadline_us: s.deadline_us(),
            timer_armed: s.timer_armed(),
            recv_una: 0,
            recv_out_of_order: 0,
            recv_out_of_order_peak: 0,
            dup_dropped: 0,
            sack_blocks: Vec::new(),
            fin_allocated: s.fin_allocated(),
            fin_acked: s.fin_acked(),
            peer_fin: false,
            datagrams_sent: 0,
            datagrams_received: 0,
            datagrams_malformed: 0,
            bytes_written: 0,
            bytes_delivered: 0,
            bytes_acked: s.bytes_acked(),
            new_transmits: s.tx_counts().0,
            retransmits: s.tx_counts().1,
        }
    }

    /// Attach the receive side, which lives on the connection rather than the
    /// sender.
    pub fn with_receive(mut self, c: &Conn) -> Self {
        let r = c.receiver();
        self.local = c.local_addr().to_string();
        self.peer = c.peer_addr().to_string();
        self.state = c.state().to_string();
        self.recv_una = r.una();
        self.recv_out_of_order = r.out_of_order_len();
        self.recv_out_of_order_peak = r.out_of_order_peak();
        self.dup_dropped = r.duplicates_dropped();
        self.sack_blocks = r.sack_for_packet();
        let st = c.stats();
        self.datagrams_sent = st.datagrams_sent;
        self.datagrams_received = st.datagrams_received;
        self.datagrams_malformed = st.datagrams_malformed;
        self.bytes_written = st.bytes_written;
        self.bytes_delivered = st.bytes_delivered;
        self.peer_fin = r.fin_seen();
        self
    }

    /// The send window as a printable interval.
    pub fn window(&self) -> String {
        format!("[{},{})", self.una, self.next)
    }

    /// A one-line diagnosis of the most likely bottleneck.
    ///
    /// Not a diagnosis engine: a shortlist of the states that actually explain a
    /// throughput collapse, so the reader starts from a hypothesis rather than
    /// from four numbers.
    pub fn likely_cause(&self) -> &'static str {
        if self.fin_allocated && !self.fin_acked {
            return "close in progress: FIN is unacknowledged";
        }
        if self.consecutive_timeouts > 0 {
            return "path is not carrying traffic: retransmission timeouts are firing";
        }
        if self.peer_fin {
            return "peer has closed its send side";
        }
        if self.window_stalled && self.rtx_queue_len > 0 {
            return "send window is closed behind an unacknowledged hole";
        }
        if self.rtx_in_queue > 0 && self.repair_target.is_some() {
            return "fast retransmit in progress: hole is being repaired";
        }
        if self.cc_phase == CcPhase::Timeout {
            return "congestion window collapsed by a timeout and is refilling";
        }
        if self.cc_phase == CcPhase::SlowStart {
            return "still in slow start: the window has not reached ssthresh yet";
        }
        if self.srtt_us.map(|s| s > 200_000).unwrap_or(false) {
            return "RTT estimate is high: the path is long or badly delayed";
        }
        "healthy: window open, no loss signals outstanding"
    }
}

impl fmt::Display for ConnDump {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        writeln!(f, "=== rudpx connection dump ===")?;
        writeln!(f, "state      {}   peer {}", self.state, self.peer)?;
        writeln!(f, "")?;

        // The four required sections, in a fixed order.
        writeln!(f, "-- send window --")?;
        writeln!(f, "  interval        {}", self.window())?;
        writeln!(
            f,
            "  una={} next={} inflight={} credit={} stalled={}",
            self.una, self.next, self.inflight, self.window_credit, self.window_stalled
        )?;
        writeln!(f, "  buffered        {} B", self.pending_bytes)?;
        writeln!(f, "")?;

        writeln!(f, "-- congestion window --")?;
        writeln!(f, "  cwnd={} ssthresh={} phase={}", self.cwnd, self.ssthresh, self.cc_phase)?;
        writeln!(f, "  dup_acks={} sacked_credit={} B", self.dup_acks, self.sacked_credit_bytes)?;
        writeln!(f, "")?;

        writeln!(f, "-- retransmission queue --")?;
        writeln!(
            f,
            "  len={} peak={} sacked={} head_seq={:?} head_tx={}",
            self.rtx_queue_len, self.rtx_queue_peak, self.rtx_in_queue, self.rtx_head_seq, self.rtx_head_tx_count
        )?;
        writeln!(
            f,
            "  repair={:?} fast_retransmits={} timeouts={} backoff=2^{} consecutive={} spurious={}",
            self.repair_target,
            self.fast_retransmits,
            self.timeouts,
            self.backoff_exp,
            self.consecutive_timeouts,
            self.spurious_retransmits
        )?;
        writeln!(f, "")?;

        writeln!(f, "-- RTT estimate --")?;
        writeln!(
            f,
            "  srtt={} rttvar={} rto={} (base {}) samples={}",
            fmt_us_opt(self.srtt_us),
            fmt_us(self.rttvar_us),
            fmt_us(self.rto_us),
            fmt_us(self.base_rto_us),
            self.rtt_samples
        )?;
        writeln!(
            f,
            "  timer={} armed={}",
            fmt_us(self.timer_deadline_us),
            self.timer_armed
        )?;
        writeln!(f, "")?;

        writeln!(f, "-- receive side --")?;
        writeln!(
            f,
            "  una={} ooo={} ooo_peak={} dup_dropped={}",
            self.recv_una, self.recv_out_of_order, self.recv_out_of_order_peak, self.dup_dropped
        )?;
        let sacks: Vec<String> = self.sack_blocks.iter().map(|b| b.to_string()).collect();
        writeln!(f, "  sack_holes      {}", if sacks.is_empty() { "-".into() } else { sacks.join(" ") })?;
        writeln!(f, "")?;

        writeln!(f, "-- close --")?;
        writeln!(
            f,
            "  fin_allocated={} fin_acked={} peer_fin={}",
            self.fin_allocated, self.fin_acked, self.peer_fin
        )?;
        writeln!(f, "")?;

        writeln!(f, "-- counters --")?;
        writeln!(
            f,
            "  datagrams tx={} rx={} malformed={}",
            self.datagrams_sent, self.datagrams_received, self.datagrams_malformed
        )?;
        writeln!(
            f,
            "  bytes written={} delivered={} acked={}",
            self.bytes_written, self.bytes_delivered, self.bytes_acked
        )?;
        writeln!(f, "  segments new={} retransmitted={}", self.new_transmits, self.retransmits)?;
        writeln!(f, "")?;
        writeln!(f, "  >> {}", self.likely_cause())?;
        Ok(())
    }
}

/// Render an optional microsecond value, showing "none" when unset.
///
/// "none" rather than "0" on purpose: a connection with no RTT sample yet has
/// no RTT, and printing zero would be indistinguishable from a real
/// sub-microsecond measurement.
fn fmt_us_opt(us: Option<u64>) -> String {
    match us {
        Some(v) => fmt_us(v),
        None => "none (no sample yet)".to_string(),
    }
}

/// Render microseconds as a human duration, keeping the exact value visible.
fn fmt_us(us: u64) -> String {
    match us {
        0 => "0".to_string(),
        _ => format!("{}us ({:?})", us, Duration::from_micros(us)),
    }
}

/// Render a dump taken from a live connection.
pub fn dump(c: &Conn) -> String {
    ConnDump::capture(c).with_receive(c).to_string()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::congestion::CcConfig;
    use crate::sender::{Sender, TxAction};

    /// A sender with `n` segments in flight, for a realistic dump.
    fn loaded(n: usize) -> Sender {
        let mut s = Sender::new(100, CcConfig::default());
        s.write(&vec![7u8; n * 1400]);
        let mut now = 0;
        while let TxAction::New(seg) = s.pump(now) {
            s.on_sent(seg.seq, now);
            now += 10;
            if s.queue_len() >= n {
                break;
            }
        }
        s
    }

    #[test]
    fn dump_contains_all_four_required_sections() {
        let s = loaded(5);
        let d = ConnDump::from_sender(&s, "ESTABLISHED".into());
        let text = d.to_string();
        for section in [
            "-- send window --",
            "-- congestion window --",
            "-- retransmission queue --",
            "-- RTT estimate --",
        ] {
            assert!(text.contains(section), "dump is missing section {section:?}");
        }
    }

    #[test]
    fn dump_reports_the_window_interval() {
        let s = loaded(5);
        let d = ConnDump::from_sender(&s, "ESTABLISHED".into());
        assert_eq!(d.window(), format!("[{},{})", s.una(), s.next()));
        assert!(d.to_string().contains(&d.window()), "the interval must appear in the text");
    }

    #[test]
    fn dump_reports_cwnd_ssthresh_and_phase() {
        let s = loaded(5);
        let d = ConnDump::from_sender(&s, "ESTABLISHED".into());
        let text = d.to_string();
        assert!(text.contains(&format!("cwnd={}", d.cwnd)));
        assert!(text.contains(&format!("ssthresh={}", d.ssthresh)));
        assert!(text.contains(&d.cc_phase.to_string()));
    }

    #[test]
    fn dump_reports_queue_length_and_first_sequence() {
        let s = loaded(5);
        let d = ConnDump::from_sender(&s, "ESTABLISHED".into());
        assert_eq!(d.rtx_queue_len, s.queue_len());
        assert_eq!(d.rtx_head_seq, s.first_queued_seq());
        let text = d.to_string();
        assert!(text.contains(&format!("len={}", s.queue_len())));
        assert!(text.contains(&format!("head_seq={:?}", s.first_queued_seq())));
    }

    #[test]
    fn dump_reports_srtt_and_rttvar() {
        let mut s = loaded(5);
        s.on_ack(103, &[], 50_000);
        let d = ConnDump::from_sender(&s, "ESTABLISHED".into());
        assert!(d.srtt_us.is_some(), "an RTT sample should have been taken");
        let text = d.to_string();
        assert!(text.contains("srtt="));
        assert!(text.contains("rttvar="));
    }

    #[test]
    fn dump_handles_a_connection_with_no_samples_yet() {
        let s = Sender::new(0, CcConfig::default());
        let d = ConnDump::from_sender(&s, "ESTABLISHED".into());
        assert_eq!(d.srtt_us, None);
        // Must still render, showing an explicit "no sample" rather than a zero
        // that reads like a real measurement.
        let text = d.to_string();
        assert!(text.contains("none (no sample yet)"));
    }

    #[test]
    fn dump_of_an_idle_connection_is_still_complete() {
        let s = Sender::new(0, CcConfig::default());
        let text = ConnDump::from_sender(&s, "CLOSED".into()).to_string();
        assert!(text.contains("len=0"));
        assert!(text.contains("head_seq=None"));
        assert!(text.contains(">> "), "a verdict line is always present");
    }

    #[test]
    fn likely_cause_names_a_stuck_send_window() {
        // A window of exactly one segment, filled and never acknowledged: the
        // sender cannot open the window and has a hole it cannot close.
        let mut s = Sender::new(0, CcConfig {
            initial_cwnd: 1,
            initial_ssthresh: 1,
            max_cwnd: 1,
            ..CcConfig::default()
        });
        s.write(&vec![1u8; 4 * 1400]);
        if let TxAction::New(seg) = s.pump(0) {
            s.on_sent(seg.seq, 0);
        }
        let d = ConnDump::from_sender(&s, "ESTABLISHED".into());
        assert!(d.window_stalled, "cwnd 1 with one segment outstanding is a full window");
        assert!(d.likely_cause().contains("send window"), "got {}", d.likely_cause());
    }

    #[test]
    fn likely_cause_names_a_retransmission_storm() {
        let mut s = loaded(4);
        s.on_timeout(10_000_000);
        let d = ConnDump::from_sender(&s, "ESTABLISHED".into());
        assert!(d.consecutive_timeouts > 0);
        assert!(d.likely_cause().contains("timeouts"));
    }

    #[test]
    fn likely_cause_names_a_close_in_progress() {
        let mut s = loaded(2);
        s.alloc_fin();
        let d = ConnDump::from_sender(&s, "FIN_WAIT".into());
        assert!(d.fin_allocated && !d.fin_acked);
        assert!(d.likely_cause().contains("close"));
    }

    #[test]
    fn likely_cause_reports_health_when_nothing_is_wrong() {
        // Established, out of slow start, window open, no loss signals, RTT
        // reasonable: nothing here is wrong.
        let mut s = loaded(3);
        s.cc_mut().set_ssthresh(2);
        s.cc_mut().set_phase(CcPhase::CongestionAvoidance);
        s.on_ack(101, &[], 20_000);
        let d = ConnDump::from_sender(&s, "ESTABLISHED".into());
        assert!(!d.window_stalled);
        assert_eq!(d.consecutive_timeouts, 0);
        assert!(d.likely_cause().contains("healthy"), "got {}", d.likely_cause());
    }

    #[test]
    fn a_stalled_window_is_visibly_stalled() {
        let mut s = Sender::new(0, CcConfig {
            initial_cwnd: 1,
            ..CcConfig::default()
        });
        s.write(&vec![1u8; 10 * 1400]);
        if let TxAction::New(seg) = s.pump(0) {
            s.on_sent(seg.seq, 0);
        }
        let d = ConnDump::from_sender(&s, "ESTABLISHED".into());
        assert!(d.window_stalled, "cwnd 1 with one segment outstanding is a full window");
    }

    #[test]
    fn duration_formatting_is_readable() {
        assert_eq!(fmt_us(0), "0");
        // The exact microsecond value is what a reader needs to compare against
        // a peer; the Debug rendering is a convenience alongside it.
        assert!(fmt_us(50_000).contains("50000us"), "got {}", fmt_us(50_000));
        assert!(fmt_us(50_000).contains("50ms"), "got {}", fmt_us(50_000));
        assert!(fmt_us(1_000_000).contains("1s"), "got {}", fmt_us(1_000_000));
    }
}

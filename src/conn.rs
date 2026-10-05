//! Connection lifecycle: the state machine, the buffers, and the engine that
//! drives a [`UdpSocket`].
//!
//! [`Conn`] is the type an application actually holds. It owns a sender, a
//! receiver, and — when it is a live connection — the socket to use. Everything
//! the transport does happens in one of three places:
//!
//! * [`Conn::write`] accepts application bytes into the send buffer.
//! * [`Conn::pump`] moves data in both directions. It is the only place that
//!   touches the socket, and it is non-blocking: it never waits.
//! * [`Conn::recv`] hands ordered bytes back to the application.
//!
//! A caller drives the engine from a single thread, which is the whole point of
//! the synchronous model: the ordering of "apply ACK, then send what the ACK
//! freed up" is a property of the code, not of a runtime's task scheduling. When
//! throughput collapses at 15% loss, the question is always *which* of those
//! steps is not happening, and a single-threaded loop makes that answerable.
//!
//! # Half-close
//!
//! [`Conn::shutdown`] closes only the sending side. The FIN is queued behind
//! whatever data is still buffered, so it cannot overtake in-flight bytes, and
//! the connection keeps receiving until the peer's FIN arrives. A half-closed
//! connection is a normal, fully functional state, not a degenerate one.
//!
//! # Timeouts
//!
//! Every deadline — the retransmission timer, the close handshake, the idle
//! keepalive — is checked against a [`Clock`]. With a [`SimClock`] a test can
//! drive a ten-minute link in microseconds and assert on exact timing, with no
//! sleeping and no flakiness.

use std::fmt;
use std::io;
use std::net::{SocketAddr, UdpSocket};
use std::time::Duration;

use crate::congestion::CcConfig;
use crate::receiver::{Receiver, RECV_WINDOW};
use crate::sender::{Sender, TxAction, TxSegment};
use crate::time::Clock;
use crate::wire::{
    Header, Seq, FLAG_ACK, FLAG_FIN, FLAG_RETRANS, FLAG_SYN, MAX_DATAGRAM,
};

/// Connection states.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum State {
    /// The SYN has been sent; waiting for the peer's SYN-ACK.
    SynSent,
    /// Both sides have exchanged sequence numbers.
    Established,
    /// This side has sent its FIN and is waiting for the peer's.
    FinWait,
    /// This side closed and is waiting to drain the peer's remaining data.
    Closing,
    /// The handshake is finished and no data may be sent.
    Closed,
}

impl State {
    /// Can data still be sent?
    pub fn can_send(&self) -> bool {
        matches!(self, State::SynSent | State::Established)
    }

    /// Can data still be received?
    pub fn can_recv(&self) -> bool {
        !matches!(self, State::Closed)
    }

    /// Is the connection finished?
    pub fn is_closed(&self) -> bool {
        matches!(self, State::Closed)
    }

    /// Is the connection half-closed, i.e. sending is over but receiving is not?
    pub fn is_half_closed(&self) -> bool {
        matches!(self, State::FinWait | State::Closing)
    }
}

impl fmt::Display for State {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let s = match self {
            State::SynSent => "SYN_SENT",
            State::Established => "ESTABLISHED",
            State::FinWait => "FIN_WAIT",
            State::Closing => "CLOSING",
            State::Closed => "CLOSED",
        };
        f.write_str(s)
    }
}

/// Why a [`Conn::pump`] step failed.
#[derive(Debug)]
pub enum ConnError {
    /// The socket reported an error. A UDP ICMP port-unreachable surfaces
    /// here, which on a public link usually means the peer is gone.
    Io(io::Error),
    /// A datagram arrived that is not a well-formed control packet.
    Wire(crate::wire::WireError),
    /// The peer keeps sending datagrams we cannot place in the receive window,
    /// which means it is ignoring flow control.
    Protocol(&'static str),
    /// The close handshake did not finish within its deadline.
    CloseTimeout,
    /// The peer retransmitted past every deadline; the connection is unusable.
    PeerGone,
}

impl fmt::Display for ConnError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            ConnError::Io(e) => write!(f, "socket error: {e}"),
            ConnError::Wire(e) => write!(f, "malformed datagram: {e}"),
            ConnError::Protocol(m) => write!(f, "protocol error: {m}"),
            ConnError::CloseTimeout => write!(f, "close handshake timed out"),
            ConnError::PeerGone => write!(f, "peer stopped responding"),
        }
    }
}

impl std::error::Error for ConnError {}

impl From<io::Error> for ConnError {
    fn from(e: io::Error) -> Self {
        ConnError::Io(e)
    }
}

impl From<crate::wire::WireError> for ConnError {
    fn from(e: crate::wire::WireError) -> Self {
        ConnError::Wire(e)
    }
}

/// Configuration for a connection.
#[derive(Clone, Copy, Debug)]
pub struct ConnConfig {
    pub cc: CcConfig,
    /// How long to keep retrying the close handshake before forcing the
    /// connection down. A FIN that is never acknowledged must not hold a
    /// process open forever.
    pub fin_timeout: Duration,
    /// Give up after this many consecutive retransmission timeouts.
    pub max_timeouts: u32,
    /// Out-of-order segments the receiver will buffer before considering the
    /// peer abusive. Defaults to the bitmap width.
    pub recv_window: u64,
}

impl Default for ConnConfig {
    fn default() -> Self {
        ConnConfig {
            cc: CcConfig::default(),
            fin_timeout: Duration::from_secs(30),
            max_timeouts: 12,
            recv_window: RECV_WINDOW,
        }
    }
}

/// Counters for the soak report and the state dump.
#[derive(Clone, Copy, Debug, Default)]
pub struct ConnStats {
    pub datagrams_sent: u64,
    pub datagrams_received: u64,
    pub datagrams_malformed: u64,
    pub acks_sent: u64,
    pub bytes_written: u64,
    pub bytes_delivered: u64,
    pub fast_retransmits: u32,
    pub timeouts: u32,
}

/// A reliable connection over UDP.
pub struct Conn {
    /// The socket every datagram goes out on. Owned by the connection so the
    /// engine is the only thing that touches it, which is what makes the
    /// single-threaded ordering argument hold.
    socket: UdpSocket,
    state: State,
    local: SocketAddr,
    peer: SocketAddr,
    sender: Sender,
    receiver: Receiver,
    clock: Box<dyn Clock>,
    config: ConnConfig,
    /// Reassembly scratch buffer, reused for every datagram so a connection
    /// does not allocate once per packet.
    rbuf: Vec<u8>,
    /// Length of the valid bytes in `rbuf`.
    rbuf_len: usize,
    /// Serialisation scratch buffer, likewise reused.
    sbuf: Vec<u8>,
    /// Datagrams the engine produced, when running against a simulated link.
    outbound: Vec<Vec<u8>>,
    /// When the close handshake began.
    close_started_us: Option<u64>,
    /// Whether our FIN has been acknowledged by the peer.
    fin_confirmed: bool,
    /// Whether the peer's FIN has been observed.
    peer_fin: bool,
    /// When true, datagrams are buffered for a simulated link instead of being
    /// written to the socket.
    intercept: bool,
    stats: ConnStats,
}

impl Conn {
    /// Build a connection that owns `socket` and talks to `peer`.
    pub fn new(socket: UdpSocket, peer: SocketAddr, clock: Box<dyn Clock>, config: ConnConfig) -> Self {
        // The initial sequence number is the connection's sequence origin. A
        // fixed value keeps tests reproducible; a real deployment would draw it
        // from entropy, which is why this is a parameter rather than a constant
        // baked into the sender.
        let isn = 0;
        let local = socket.local_addr().unwrap_or(peer);
        Conn {
            socket,
            state: State::Established,
            local,
            peer,
            sender: Sender::new(isn, config.cc),
            receiver: Receiver::new(isn),
            clock,
            config,
            rbuf: vec![0u8; MAX_DATAGRAM],
            rbuf_len: 0,
            sbuf: Vec::with_capacity(MAX_DATAGRAM),
            outbound: Vec::new(),
            close_started_us: None,
            fin_confirmed: false,
            peer_fin: false,
            intercept: false,
            stats: ConnStats::default(),
        }
    }

    /// Route datagrams through a simulated link instead of the socket.
    ///
    /// The connection keeps its socket for addresses and for the handshake, but
    /// outbound datagrams are buffered so a driver can damage and delay them.
    /// The inbound path is unchanged, so a simulated run exercises the same
    /// parser and state machine as a live one.
    pub fn set_intercept(&mut self, on: bool) {
        self.intercept = on;
        if !on {
            self.outbound.clear();
        }
    }

    /// Build a connection with an explicit initial sequence number, for tests
    /// and for replaying a captured trace.
    pub fn with_isn(
        socket: UdpSocket,
        peer: SocketAddr,
        clock: Box<dyn Clock>,
        config: ConnConfig,
        isn: Seq,
    ) -> Self {
        let mut c = Conn::new(socket, peer, clock, config);
        c.sender = Sender::new(isn, config.cc);
        c.receiver = Receiver::new(isn);
        c
    }

    /// Begin the handshake by sending a SYN.
    pub fn connect(&mut self) -> Result<(), ConnError> {
        self.state = State::SynSent;
        let now = self.clock.now_us();
        // A SYN-ACK from the peer establishes the connection; the initial
        // sequence number is ours, theirs is whatever they send.
        let h = Header::new(self.sender.next(), 0, now).with_flags(FLAG_SYN | FLAG_ACK);
        self.send_raw(&h, &[])?;
        Ok(())
    }

    // -- accessors ---------------------------------------------------------

    pub fn state(&self) -> State {
        self.state
    }

    pub fn local_addr(&self) -> SocketAddr {
        self.local
    }

    pub fn peer_addr(&self) -> SocketAddr {
        self.peer
    }

    pub fn sender(&self) -> &Sender {
        &self.sender
    }

    pub fn receiver(&self) -> &Receiver {
        &self.receiver
    }

    pub fn stats(&self) -> ConnStats {
        self.stats
    }

    /// The clock in use, for tests that share one across both endpoints.
    pub fn clock(&self) -> &dyn Clock {
        self.clock.as_ref()
    }

    /// Bytes accepted by [`Conn::write`] but not yet delivered to the peer.
    pub fn bytes_in_flight(&self) -> usize {
        self.sender.pending_bytes() + self.sender.queue().map(|s| s.payload.len()).sum::<usize>()
    }

    // -- application interface --------------------------------------------

    /// Write application bytes into the send buffer.
    ///
    /// Returns the number accepted. The bytes are segmented and transmitted by
    /// [`Conn::pump`]; this call only buffers, so it never blocks and never
    /// fails on a full socket.
    pub fn write(&mut self, data: &[u8]) -> usize {
        if !self.state.can_send() {
            return 0;
        }
        self.stats.bytes_written += data.len() as u64;
        self.sender.write(data)
    }

    /// Take the next in-order segment's payload, if one is deliverable.
    pub fn recv(&mut self) -> Option<Vec<u8>> {
        let p = self.receiver.take_deliverable();
        if let Some(ref v) = p {
            self.stats.bytes_delivered += v.len() as u64;
        }
        p
    }

    /// Is everything written so far acknowledged by the peer?
    pub fn is_flushed(&self) -> bool {
        self.sender.queue_len() == 0 && self.sender.pending_bytes() == 0
    }

    /// Half-close: queue a FIN behind the buffered data and stop sending.
    ///
    /// The FIN cannot overtake data because it is allocated at the current
    /// `next`, after everything already buffered. It is retransmitted like any
    /// other segment until the peer acknowledges it, so a half-close on a lossy
    /// link converges rather than being lost.
    pub fn shutdown(&mut self) -> bool {
        if !self.state.can_send() {
            return false;
        }
        // Wait for the data to be segmented first, so the FIN really does sit
        // behind it. If the buffer is still too large for the window, the FIN is
        // allocated anyway and simply waits in the queue.
        let _ = self.pump_data();
        if !self.pump_data().is_ok() || !self.sender.alloc_fin() {
            return false;
        }
        self.state = State::FinWait;
        self.close_started_us = Some(self.clock.now_us());
        true
    }

    /// Close immediately, discarding anything still in flight.
    ///
    /// Used as the forced fallback when [`Conn::shutdown`] cannot complete: a
    /// FIN that is never acknowledged must not hold a process open.
    pub fn abort(&mut self) {
        self.state = State::Closed;
    }

    /// Is the close handshake finished?
    pub fn is_closed(&self) -> bool {
        self.state == State::Closed
    }

    /// Is the peer's FIN acknowledged and our FIN confirmed, with no data left?
    pub fn is_drained(&self) -> bool {
        self.peer_fin && self.fin_confirmed && self.receiver.fin_ready() && self.is_flushed()
    }

    // -- the engine --------------------------------------------------------

    /// Make one pass over the connection: read what has arrived, act on it, and
    /// write what that freed up.
    ///
    /// Non-blocking, and bounded: it drains the socket's receive queue and stops.
    /// Returns how many datagrams were processed, so a caller can decide whether
    /// to sleep.
    pub fn pump(&mut self) -> Result<usize, ConnError> {
        if self.state == State::Closed {
            return Ok(0);
        }
        let mut handled = 0;
        // Read first: acting on an ACK can free window, and doing that before
        // reading would cost an extra loop iteration per datagram.
        loop {
            match self.recv_one() {
                Ok(true) => handled += 1,
                Ok(false) => break,
                Err(e) => return Err(e),
            }
        }
        self.pump_data()?;
        self.check_deadlines()?;
        Ok(handled)
    }

    /// Send everything the window currently allows.
    fn pump_data(&mut self) -> Result<(), ConnError> {
        // Each iteration asks the sender what to transmit and hands it to the
        // socket, so the window is refilled as fast as ACKs allow without the
        // engine ever deciding on its own what "should" be sent.
        loop {
            let now = self.clock.now_us();
            match self.sender.pump(now) {
                TxAction::New(seg) => {
                    self.transmit(&seg, false)?;
                    self.sender.on_sent(seg.seq, now);
                }
                TxAction::Retransmit { seg, reason: _ } => {
                    self.transmit(&seg, true)?;
                    self.sender.on_sent(seg.seq, now);
                }
                TxAction::Idle => break,
            }
        }
        Ok(())
    }

    /// Read one datagram and apply it. Returns false when the queue is empty.
    fn recv_one(&mut self) -> Result<bool, ConnError> {
        let n = match self.socket.recv(&mut self.rbuf) {
            Ok(n) => {
                self.rbuf_len = n;
                n
            }
            // An empty receive queue is the normal end of a pass, not an error.
            Err(e) if e.kind() == io::ErrorKind::WouldBlock => return Ok(false),
            // A datagram larger than our buffer was truncated; the header still
            // parsed, and the CRC will reject it if the truncation corrupted it.
            Err(e) if e.kind() == io::ErrorKind::InvalidInput => return Ok(false),
            Err(e) => return Err(e.into()),
        };
        if n == 0 {
            return Ok(false);
        }
        self.stats.datagrams_received += 1;
        if self.handle_datagram().is_err() {
            // A malformed datagram is counted and dropped, never fatal: a
            // corrupted or hostile packet must not take the connection down. On
            // a public link, garbage is expected traffic rather than an
            // exceptional event.
            self.stats.datagrams_malformed += 1;
        }
        Ok(true)
    }

    /// Apply the datagram sitting in `rbuf`.
    fn handle_datagram(&mut self) -> Result<(), ConnError> {
        let h = Header::decode(&self.rbuf[..self.rbuf_len])?;
        let now = self.clock.now_us();

        if h.has(FLAG_SYN) && self.state == State::SynSent {
            self.state = State::Established;
        }

        // A FIN ends the peer's stream. It is only actionable once every
        // segment before it has been delivered, so record it and let the
        // receiver decide.
        if h.has(FLAG_FIN) {
            self.peer_fin = true;
            self.receiver.on_fin(h.seq);
            if self.receiver.fin_ready() && self.state != State::Closed {
                self.state = State::Closing;
            }
        }

        // Data and acknowledgement travel in the same datagram, so the segment
        // is delivered first and the ACK it carries is applied second. Doing it
        // in this order means a segment that completes a SACK block is credited
        // in the same pass, rather than one datagram later.
        if h.has_payload(self.rbuf_len) {
            self.deliver(&h)?;
        }

        if h.has(FLAG_ACK) {
            // A loss signal, if any, is acted on by `pump_data`, which is called
            // at the end of `pump`.
            self.sender.on_ack(h.ack, &h.sack, now);
            // Reply with an ACK whenever the datagram told us something. ACKing
            // every inbound datagram unconditionally would double the packet
            // count on a healthy link; ACKing only on change is what keeps the
            // reverse direction cheap.
            if h.has_payload(self.rbuf_len) || h.has(FLAG_FIN) || !h.sack.is_empty() {
                self.maybe_send_ack()?;
            }
        }

        // Our FIN is confirmed once the cumulative ACK passes it.
        if self.sender.fin_acked() && !self.fin_confirmed {
            self.fin_confirmed = true;
        }

        Ok(())
    }

    /// Deliver a data segment from `h`, if the datagram carried one.
    fn deliver(&mut self, h: &Header) -> Result<(), ConnError> {
        let hl = h.encoded_len();
        let plen = h.payload_len(self.rbuf_len);
        if self.rbuf_len < hl + plen {
            return Err(ConnError::Protocol("payload shorter than its header claims"));
        }
        let payload = self.rbuf[hl..hl + plen].to_vec();
        self.receiver
            .on_segment(h.seq, payload, h.has(FLAG_RETRANS));
        Ok(())
    }

    /// Build and send an ACK for the current receive state.
    fn maybe_send_ack(&mut self) -> Result<(), ConnError> {
        if self.state == State::Closed {
            return Ok(());
        }
        let now = self.clock.now_us();
        let sack = self.receiver.sack_for_packet();
        // Echo the peer's FIN back so it knows its close was received. This is
        // what makes a half-close converge: without it a peer that FINs while we
        // are still sending would never learn that we saw it.
        let mut h = Header::new(0, self.receiver.una(), now)
            .with_flags(FLAG_ACK)
            .with_sack(sack);
        if self.peer_fin && self.receiver.fin_ready() {
            h.flags |= FLAG_FIN;
        }
        self.send_raw(&h, &[])?;
        self.stats.acks_sent += 1;
        if self.peer_fin {
            self.receiver.mark_fin_acked();
        }
        Ok(())
    }

    /// Transmit one segment.
    fn transmit(&mut self, seg: &TxSegment, retransmit: bool) -> Result<(), ConnError> {
        let now = self.clock.now_us();
        let mut h = Header::new(seg.seq, self.receiver.una(), now).with_flags(FLAG_ACK);
        if retransmit {
            h.flags |= FLAG_RETRANS;
        }
        if seg.fin {
            h.flags |= FLAG_FIN;
        }
        // Piggyback the SACK list on data: an ACK costs nothing extra when it
        // rides along with a segment we were sending anyway, and on a
        // window-limited connection that is most of them.
        h = h.with_sack(self.receiver.sack_for_packet());
        self.send_raw(&h, &seg.payload)
    }

    /// Encode and send one datagram, reusing the scratch buffer.
    fn send_raw(&mut self, h: &Header, payload: &[u8]) -> Result<(), ConnError> {
        self.sbuf.clear();
        self.sbuf.resize(h.encoded_len() + payload.len(), 0);
        let hl = h.encode(&mut self.sbuf);
        self.sbuf[hl..hl + payload.len()].copy_from_slice(payload);

        if self.intercept {
            // Simulated link: the driver routes this datagram through the
            // damage model and back into the peer's socket.
            self.outbound.push(self.sbuf.clone());
            self.stats.datagrams_sent += 1;
            return Ok(());
        }

        // A send that would block means the kernel buffer is full. Treating that
        // as a hard error would turn a momentary burst into a connection reset,
        // so the datagram is dropped and the retransmission timer covers it.
        match self.socket.send(&self.sbuf) {
            Ok(_) => {
                self.stats.datagrams_sent += 1;
                Ok(())
            }
            Err(e) if e.kind() == io::ErrorKind::WouldBlock => Ok(()),
            Err(e) => Err(e.into()),
        }
    }

    /// Check every deadline: retransmission, close, and give-up.
    fn check_deadlines(&mut self) -> Result<(), ConnError> {
        let now = self.clock.now_us();

        let _ = now;

        if self.state.is_half_closed() {
            if let Some(start) = self.close_started_us {
                let elapsed = Duration::from_micros(now.saturating_sub(start));
                if elapsed >= self.config.fin_timeout {
                    // A FIN that is never acknowledged must not hold a process
                    // open. Force the connection down and say so.
                    self.state = State::Closed;
                    return Err(ConnError::CloseTimeout);
                }
            }
        }

        if self.sender.consecutive_timeouts() >= self.config.max_timeouts {
            self.state = State::Closed;
            return Err(ConnError::PeerGone);
        }

        // Once both FINs have crossed, the connection is finished.
        if self.is_drained() {
            self.state = State::Closed;
        }
        Ok(())
    }

    // -- simulated-link hooks ---------------------------------------------
    //
    // A live connection sends through its socket. The simulator needs to see
    // those datagrams in order to damage them, and to hand them to the peer's
    // socket later. Rather than duplicating the framing logic, the engine
    // buffers what it wants to send and the driver routes it; these two methods
    // are that seam. A production caller never touches them.

    /// Drain the datagrams the engine produced during the last [`Conn::pump`].
    pub fn take_outbound(&mut self) -> Vec<Vec<u8>> {
        std::mem::take(&mut self.outbound)
    }

    /// Hand the connection a datagram that arrived from the peer.
    ///
    /// Uses the same path as a socket receive, so a simulated run exercises the
    /// real parsing and state transitions rather than a parallel code path that
    /// could drift from it.
    pub fn feed_inbound(&mut self, datagram: &[u8]) {
        if datagram.len() > self.rbuf.len() {
            // Larger than any datagram we can build; it cannot be ours.
            self.stats.datagrams_malformed += 1;
            return;
        }
        self.rbuf[..datagram.len()].copy_from_slice(datagram);
        self.rbuf_len = datagram.len();
        self.stats.datagrams_received += 1;
        if self.handle_datagram().is_err() {
            self.stats.datagrams_malformed += 1;
        }
    }

    /// Is there anything the engine wants to send?
    pub fn has_outbound(&self) -> bool {
        !self.outbound.is_empty()
    }

    /// A duplicate handle on the connection's socket.
    ///
    /// A driver needs this to route delivered datagrams into the connection
    /// without taking a mutable borrow of the connection itself. Handles are
    /// cheap and independent, so this is how the simulated link reaches the
    /// kernel queue.
    pub fn try_socket(&self) -> std::io::Result<UdpSocket> {
        self.socket.try_clone()
    }

    /// How long until the next deadline, for a caller that wants to sleep.
    pub fn poll_timeout(&self) -> Option<Duration> {
        if self.state == State::Closed {
            return None;
        }
        let now = self.clock.now_us();
        if self.sender.timer_armed() {
            return Some(Duration::from_micros(self.sender.time_to_deadline_us(now).max(1)));
        }
        if self.state.is_half_closed() {
            if let Some(start) = self.close_started_us {
                let left = self.config.fin_timeout.saturating_sub(Duration::from_micros(now.saturating_sub(start)));
                if !left.is_zero() {
                    return Some(left);
                }
            }
        }
        None
    }
}


impl fmt::Debug for Conn {
    /// A one-line summary, deliberately not a field-by-field dump.
    ///
    /// The full state is what [`crate::dump`] is for; this exists so a `Conn`
    /// can appear in a `{:?}` in a test failure or a log line without dragging
    /// the whole send and receive state along. The socket and the clock are
    /// omitted because neither says anything useful about the connection.
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Conn")
            .field("state", &self.state)
            .field("peer", &self.peer)
            .field("window", &self.sender.window().to_string())
            .field("cwnd", &self.sender.cc().cwnd())
            .field("phase", &self.sender.cc().phase().to_string())
            .field("srtt_us", &self.sender.rtt().srtt())
            .field("rtx_queue", &self.sender.queue_len())
            .finish()
    }
}

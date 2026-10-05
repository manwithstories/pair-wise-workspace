//! **rudpx** — a reliable, ordered byte stream over `std::net::UdpSocket`.
//!
//! Selective acknowledgement, Reno congestion control, reproducible link
//! damage, graceful half-close, and a runtime state dump. No third-party
//! dependencies and no async runtime: the whole transport is `std`, synchronous,
//! and driven by the caller from one thread.
//!
//! # Using it
//!
//! ```no_run
//! use std::net::UdpSocket;
//! use rudpx::conn::{Conn, ConnConfig};
//! use rudpx::time::RealClock;
//!
//! let socket = UdpSocket::bind("0.0.0.0:9000")?;
//! let peer = "203.0.113.7:9000".parse()?;
//! let mut conn = Conn::new(socket, peer, Box::new(RealClock::new()), ConnConfig::default());
//!
//! conn.write(b"log batch");
//! loop {
//!     conn.pump()?;                 // non-blocking: read, act, send
//!     while let Some(bytes) = conn.recv() {
//!         let _ = bytes;            // ordered, exactly as written
//!     }
//!     std::thread::sleep(conn.poll_timeout().unwrap_or_default());
//! }
//! # Ok::<(), Box<dyn std::error::Error>>(())
//! ```
//!
//! # Module map
//!
//! | Module | Role |
//! | --- | --- |
//! | [`wire`] | On-wire framing, sequence arithmetic, CRC32 |
//! | [`conn`] | Connection state machine, buffers, the engine driving the socket |
//! | [`sender`] | Send window, segment allocation, retransmission queue, timers |
//! | [`receiver`] | Reassembly, SACK block construction, duplicate detection |
//! | [`congestion`] | Reno: slow start, congestion avoidance, fast retransmit/recovery |
//! | [`loss_injector`] | Seeded SplitMix64 damage: loss, reordering, duplication |
//! | [`dump`] | Human-readable connection state dump |
//! | [`time`] | Clock abstraction and RFC 6298 RTT estimation |
//! | [`sim`], [`harness`] | Simulated link and a two-endpoint test harness |
//!
//! The first eight are the transport. [`sim`] and [`harness`] exist so the
//! engine can be exercised end to end — both endpoints, real sockets, real
//! damage, virtual time — without threads or sleeping; nothing in the data path
//! depends on them.
//!
//! # What it guarantees
//!
//! Bytes are delivered to the application in the order they were written, and a
//! transfer either completes intact or the connection reports why it cannot. The
//! test suite checks that at 0%, 1%, 5%, 10%, and 15% loss, across several
//! seeds, with reordering and duplication active at the same time.
//!
//! # What it does not
//!
//! Reno's additive-increase / multiplicative-decrease cycle settles around
//! `1.2/sqrt(p)` segments, so on a path losing 15% of its packets the window
//! stays small. The transport stays correct and does not collapse, but the
//! throughput is Reno's, not the link's. See the README for measured figures
//! and for where a different algorithm would drop in.

#![forbid(unsafe_code)]
#![warn(missing_debug_implementations)]

pub mod congestion;
pub mod conn;
pub mod dump;
pub mod harness;
pub mod loss_injector;
pub mod receiver;
pub mod sender;
pub mod sim;
pub mod time;
pub mod wire;

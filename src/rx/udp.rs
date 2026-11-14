use crate::model::{Frame, Packet};
use std::net::SocketAddr;
use std::time::{Duration, Instant};
use thiserror::Error;
use tokio::net::UdpSocket;

pub const MAX_DATAGRAM_BYTES: usize = 4_096;
pub const RECEIVE_TIMEOUT_MS: u64 = 2_000;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum TimingMode {
    Monotonic,
    Embedded { start_us: u64 },
}

pub struct UdpReceiver {
    socket: UdpSocket,
    timing: TimingMode,
    started: Instant,
    last_arrival_us: u64,
}

impl UdpReceiver {
    pub async fn bind(addr: SocketAddr, timing: TimingMode) -> Result<Self, UdpError> {
        require_loopback(addr)?;
        let socket = UdpSocket::bind(addr).await.map_err(UdpError::Io)?;
        if !socket
            .local_addr()
            .map_err(UdpError::Io)?
            .ip()
            .is_loopback()
        {
            return Err(UdpError::ForbiddenAddress(addr));
        }
        Ok(Self {
            socket,
            timing,
            started: Instant::now(),
            last_arrival_us: 0,
        })
    }

    pub fn local_addr(&self) -> Result<SocketAddr, UdpError> {
        self.socket.local_addr().map_err(UdpError::Io)
    }

    pub async fn recv(&mut self) -> Result<Frame, UdpError> {
        let mut bytes = vec![0u8; MAX_DATAGRAM_BYTES];
        let (len, _) = self
            .socket
            .recv_from(&mut bytes)
            .await
            .map_err(UdpError::Io)?;
        let packet = Packet::decode_udp(&bytes[..len]).map_err(UdpError::Packet)?;
        let arrival_us = match self.timing {
            TimingMode::Monotonic => self.started.elapsed().as_micros() as u64,
            TimingMode::Embedded { start_us } => {
                let arrival = start_us.saturating_add(packet.arrival_us);
                if arrival < self.last_arrival_us {
                    return Err(UdpError::TimestampRollback {
                        previous: self.last_arrival_us,
                        current: arrival,
                    });
                }
                arrival
            }
        };
        self.last_arrival_us = arrival_us;
        let mut packet = packet;
        packet.arrival_us = arrival_us;
        Frame::from_packet(packet).map_err(UdpError::Packet)
    }

    pub async fn recv_timeout(&mut self) -> Result<Option<Frame>, UdpError> {
        match tokio::time::timeout(Duration::from_millis(RECEIVE_TIMEOUT_MS), self.recv()).await {
            Ok(result) => result.map(Some),
            Err(_) => Ok(None),
        }
    }
}

pub async fn bind_sender(target: SocketAddr) -> Result<UdpSocket, UdpError> {
    require_loopback(target)?;
    let local = if target.is_ipv4() {
        "127.0.0.1:0"
    } else {
        "[::1]:0"
    };
    let socket = UdpSocket::bind(local).await.map_err(UdpError::Io)?;
    Ok(socket)
}

pub async fn send_packet(
    socket: &UdpSocket,
    target: SocketAddr,
    packet: &Packet,
) -> Result<(), UdpError> {
    let wire = packet.encode_udp().map_err(UdpError::Packet)?;
    socket.send_to(&wire, target).await.map_err(UdpError::Io)?;
    Ok(())
}

pub fn require_loopback(addr: SocketAddr) -> Result<(), UdpError> {
    if addr.ip().is_loopback() {
        Ok(())
    } else {
        Err(UdpError::ForbiddenAddress(addr))
    }
}

#[derive(Debug, Error)]
pub enum UdpError {
    #[error("I/O error: {0}")]
    Io(#[from] std::io::Error),
    #[error("UDP is loopback-only; refusing {0}")]
    ForbiddenAddress(SocketAddr),
    #[error("invalid UDP packet: {0}")]
    Packet(#[from] crate::model::ModelError),
    #[error("UDP timestamp rollback: {previous} -> {current} us")]
    TimestampRollback { previous: u64, current: u64 },
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::FRAME_SAMPLES;

    #[test]
    fn rejects_non_loopback_addresses() {
        let public: SocketAddr = "0.0.0.0:9000".parse().unwrap();
        assert!(matches!(
            require_loopback(public),
            Err(UdpError::ForbiddenAddress(_))
        ));
    }

    #[test]
    fn loopback_wire_round_trip_uses_embedded_clock() {
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();
        runtime.block_on(async {
            let addr = "127.0.0.1:0".parse().unwrap();
            let mut receiver = UdpReceiver::bind(addr, TimingMode::Embedded { start_us: 0 })
                .await
                .unwrap();
            let addr = receiver.local_addr().unwrap();
            let sender = bind_sender(addr).await.unwrap();
            let packet = Packet {
                ssrc: 1,
                seq: 7,
                ts: 6_720,
                arrival_us: 123_000,
                pcm: vec![42; FRAME_SAMPLES],
            };
            send_packet(&sender, addr, &packet).await.unwrap();
            let frame = receiver.recv().await.unwrap();
            assert_eq!(frame.seq, 7);
            assert_eq!(frame.arrival_us, 123_000);
        });
    }

    #[test]
    fn embedded_clock_rejects_rollback() {
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();
        runtime.block_on(async {
            let mut receiver = UdpReceiver::bind(
                "127.0.0.1:0".parse().unwrap(),
                TimingMode::Embedded { start_us: 0 },
            )
            .await
            .unwrap();
            let addr = receiver.local_addr().unwrap();
            let sender = bind_sender(addr).await.unwrap();
            for arrival in [100_000u64, 50_000] {
                let packet = Packet {
                    ssrc: 1,
                    seq: 1,
                    ts: 0,
                    arrival_us: arrival,
                    pcm: vec![0; FRAME_SAMPLES],
                };
                send_packet(&sender, addr, &packet).await.unwrap();
            }
            assert!(receiver.recv().await.is_ok());
            assert!(matches!(
                receiver.recv().await,
                Err(UdpError::TimestampRollback { .. })
            ));
        });
    }
}

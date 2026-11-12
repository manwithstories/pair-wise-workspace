//! `secpipe`: a local, replayable and auditable security log pipeline.
//!
//! Module dependency direction is strictly one way. Every layer depends only on
//! [`config`] and earlier layers:
//! `fixtures -> parser -> clock -> store -> window/rules/session/anomaly -> report`.

pub mod anomaly;
pub mod clock;
pub mod config;
pub mod error;
pub mod fixtures;
pub mod model;
pub mod parser;
pub mod pipeline;
pub mod report;
pub mod rng;
pub mod rules;
pub mod session;
pub mod store;
pub mod stress;
pub mod window;

pub use error::{Error, Result};

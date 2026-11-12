//! Crate-wide error type. Every fallible boundary returns [`Error`] so that
//! `main` can map failures to stable, non-zero process exit codes.

use std::io;
use std::path::PathBuf;

#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("io error on {path}: {source}")]
    Io {
        path: PathBuf,
        #[source]
        source: io::Error,
    },

    #[error("configuration error: {0}")]
    Config(String),

    #[error("parse error on {file} line {line}: {reason}")]
    Parse {
        file: String,
        line: usize,
        reason: String,
    },

    #[error("clock ambiguity: {0}")]
    Clock(String),

    #[error("sqlite error: {0}")]
    Sqlite(#[from] rusqlite::Error),

    #[error("json error: {0}")]
    Json(#[from] serde_json::Error),

    #[error("toml error: {0}")]
    Toml(String),

    #[error("self-check failure: {0}")]
    SelfCheck(String),

    #[error("{0}")]
    Other(String),
}

impl Error {
    /// Stable process exit codes used by the CLI.
    pub fn exit_code(&self) -> i32 {
        match self {
            Error::Config(_) => 2,
            Error::Clock(_) => 3,
            Error::Parse { .. } => 4,
            Error::SelfCheck(_) => 5,
            _ => 1,
        }
    }
}

pub type Result<T> = std::result::Result<T, Error>;

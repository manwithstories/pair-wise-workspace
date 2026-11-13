use std::path::PathBuf;
use std::process::ExitCode;

use clap::{Parser, Subcommand};
use soc_correlator::gen::{self, DEFAULT_RAW_LINES};
use soc_correlator::pipeline;
use soc_correlator::report;

#[derive(Debug, Parser)]
#[command(
    name = "soc-correlator",
    version,
    about = "Offline DHCP/NAT/EDR correlation and anomaly detection"
)]
struct Cli {
    #[command(subcommand)]
    command: Command,
}

#[derive(Debug, Subcommand)]
enum Command {
    /// Generate fixtures, ingest them, correlate, detect, and write report.json.
    Demo {
        #[arg(long, default_value_t = 42)]
        seed: u64,
        #[arg(long, default_value = "./out")]
        out: PathBuf,
        /// Total raw log rows (accepted + malformed). Defaults to the acceptance corpus.
        #[arg(long, default_value_t = DEFAULT_RAW_LINES)]
        raw_lines: usize,
    },
    /// Parse and ingest a generated fixture directory into SQLite.
    Ingest {
        #[arg(long, default_value = "./out/fixtures")]
        input: PathBuf,
        #[arg(long, default_value = "./out/security.db")]
        db: PathBuf,
        /// Explicitly reopen previously closed windows and reprocess late rows.
        #[arg(long)]
        reopen_closed: bool,
    },
    /// Read SQLite events, run rules/anomaly detection, and write report.json.
    Report {
        #[arg(long, default_value = "./out/security.db")]
        db: PathBuf,
        #[arg(long = "out", default_value = "./out/report.json")]
        output: PathBuf,
    },
}

#[derive(Debug, thiserror::Error)]
enum AppError {
    #[error("fixture generation failed: {0}")]
    Gen(#[from] gen::GenError),
    #[error("ingest failed: {0}")]
    Ingest(#[from] pipeline::PipelineError),
    #[error("report failed: {0}")]
    Report(#[from] report::ReportError),
}

fn main() -> ExitCode {
    let cli = Cli::parse();
    match run(cli) {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("错误: {error}");
            ExitCode::from(1)
        }
    }
}

fn run(cli: Cli) -> Result<(), AppError> {
    match cli.command {
        Command::Demo {
            seed,
            out,
            raw_lines,
        } => {
            let fixtures = out.join("fixtures");
            let database = out.join("security.db");
            let json_path = out.join("report.json");
            let manifest = gen::generate(seed, &fixtures, raw_lines)?;
            println!(
                "夹具 {} 批次={} 原始行={} 可解析={} 畸形={} 迟到注入={}",
                fixtures.display(),
                manifest.batches,
                manifest.raw_lines,
                manifest.accepted_lines,
                manifest.malformed_lines,
                manifest.late_injected
            );
            let stats = pipeline::ingest(&fixtures, &database, false)?;
            println!(
                "入库 批={} 新增={} 重复={} 迟到={} 隔离={} 回绕={} 身份冲突={}",
                stats.batches,
                stats.inserted_events,
                stats.duplicate_events,
                stats.late_dropped,
                stats.quarantined,
                stats.new_wraps,
                stats.new_identity_conflicts
            );
            let display = json_path.display().to_string();
            report::build_and_write(&database, &json_path, &display)?;
        }
        Command::Ingest {
            input,
            db,
            reopen_closed,
        } => {
            let stats = pipeline::ingest(&input, &db, reopen_closed)?;
            println!(
                "入库完成 批={} 可解析={} 拒绝={} 新增={} 重复={} 迟到={} 隔离={} 重开窗口={}",
                stats.batches,
                stats.accepted_parsed,
                stats.rejected_parsed,
                stats.inserted_events,
                stats.duplicate_events,
                stats.late_dropped,
                stats.quarantined,
                stats.reopened_windows
            );
        }
        Command::Report { db, output } => {
            let display = output.display().to_string();
            report::build_and_write(&db, &output, &display)?;
        }
    }
    Ok(())
}

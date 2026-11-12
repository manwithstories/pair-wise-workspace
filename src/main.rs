//! Command line interface. Exit codes are stable and documented in
//! [`secpipe::Error::exit_code`].

use std::path::{Path, PathBuf};
use std::process::ExitCode;

use clap::{Parser, Subcommand};
use secpipe::config::Config;
use secpipe::error::Error;
use secpipe::fixtures;
use secpipe::pipeline::{self, RunContext};
use secpipe::report;

#[derive(Parser, Debug)]
#[command(
    name = "secpipe",
    version,
    about = "Local auditable security log pipeline"
)]
struct Cli {
    #[command(subcommand)]
    command: Command,
}

#[derive(Subcommand, Debug)]
enum Command {
    /// Generate deterministic labeled fixtures.
    Ingest {
        #[arg(long)]
        config: PathBuf,
        #[arg(long)]
        logs: PathBuf,
        #[arg(long)]
        db: PathBuf,
    },
    /// Recompute windows, rules, sessions and anomalies.
    Correlate {
        #[arg(long)]
        config: PathBuf,
        #[arg(long)]
        db: PathBuf,
    },
    /// Render terminal/JSON/CSV reports.
    Report {
        #[arg(long)]
        config: PathBuf,
        #[arg(long)]
        db: PathBuf,
        #[arg(long)]
        logs: PathBuf,
        #[arg(long)]
        out: PathBuf,
        #[arg(long)]
        self_check: bool,
    },
    /// Throughput harness over a large synthetic dataset.
    Stress {
        #[arg(long)]
        config: PathBuf,
        #[arg(long)]
        out: PathBuf,
        #[arg(long, default_value_t = 500_000)]
        rows: u64,
    },
    /// Full pipeline: fixtures -> ingest -> correlate -> report (recommended).
    All {
        #[arg(long)]
        config: PathBuf,
        #[arg(long)]
        seed: u64,
        #[arg(long)]
        out: PathBuf,
        #[arg(long)]
        self_check: bool,
    },
}

fn main() -> ExitCode {
    let cli = Cli::parse();
    match run(cli) {
        Ok(()) => ExitCode::SUCCESS,
        Err(err) => {
            eprintln!("secpipe: {err}");
            ExitCode::from(err.exit_code() as u8)
        }
    }
}

fn run(cli: Cli) -> secpipe::Result<()> {
    match cli.command {
        Command::Ingest { config, logs, db } => {
            let cfg = Config::load(&config)?;
            let ctx = RunContext::new(cfg, db.parent().unwrap_or(Path::new(".")));
            let stats = pipeline::ingest(&ctx, &logs)?;
            println!(
                "inserted={} duplicate={} quarantined={} late_events={} restatements={} wraparounds={}",
                stats.inserted,
                stats.duplicate,
                stats.quarantined,
                stats.late_events,
                stats.restatements,
                stats.wraparounds
            );
            Ok(())
        }
        Command::Correlate { config, db } => {
            let cfg = Config::load(&config)?;
            let ctx = RunContext::new(cfg, db.parent().unwrap_or(Path::new(".")));
            let correlation = pipeline::correlate(&ctx)?;
            println!(
                "windows={} rules_hit={} anomalous_sessions={}",
                correlation.windows,
                correlation.rule_hits.len(),
                correlation.anomalous_sessions
            );
            Ok(())
        }
        Command::Report {
            config,
            db,
            logs,
            out,
            self_check,
        } => {
            let cfg = Config::load(&config)?;
            let ctx = RunContext::new(cfg, db.parent().unwrap_or(Path::new(".")));
            produce_report(&ctx, &logs, &out, self_check)?;
            Ok(())
        }
        Command::Stress { config, out, rows } => {
            let cfg = Config::load(&config)?;
            std::fs::create_dir_all(&out).map_err(|source| Error::Io {
                path: out.clone(),
                source,
            })?;
            let (elapsed_ms, windows) = secpipe::stress::run(&cfg, &out, rows)?;
            println!(
                "stress rows={} windows={} elapsed_ms={} throughput_rows_per_s={}",
                rows,
                windows,
                elapsed_ms,
                (rows as f64 / (elapsed_ms.max(1) as f64)) * 1000.0
            );
            Ok(())
        }
        Command::All {
            config,
            seed,
            out,
            self_check,
        } => {
            let cfg = Config::load(&config)?;
            std::fs::create_dir_all(&out).map_err(|source| Error::Io {
                path: out.clone(),
                source,
            })?;
            let log_dir = out.join("logs");
            let generation = fixtures::generate(&log_dir, seed)?;
            let ctx = RunContext::new(cfg.clone(), &out);

            // Clean primary ingestion; these are the reported counters.
            let stats = pipeline::ingest(&ctx, &log_dir)?;
            let store = secpipe::store::Store::open(&ctx.db_path)?;
            let counters = pipeline::snapshot(&store, &stats)?;
            drop(store);

            // Idempotency proven on an isolated scratch DB so the primary clock
            // and lateness counters stay untouched.
            let dedup_stable = verify_dedup(&cfg, &out, &log_dir)?;

            let correlation = pipeline::correlate(&ctx)?;
            let truth = load_truth(&generation.truth)?;

            let pass = if self_check {
                pipeline::self_check(&ctx, &correlation).is_ok() && dedup_stable
            } else {
                true
            };
            if self_check && !pass {
                return Err(Error::SelfCheck(
                    "self-check or duplicate-import stability failed".into(),
                ));
            }

            let metrics =
                pipeline::evaluate_metrics(&correlation, &truth, &counters, dedup_stable, pass);
            report::write_artifacts(&out, &metrics)?;
            println!("{}", metrics.summary_line());
            Ok(())
        }
    }
}

fn load_truth(path: &Path) -> secpipe::Result<fixtures::Truth> {
    let bytes = std::fs::read(path).map_err(|source| Error::Io {
        path: path.to_path_buf(),
        source,
    })?;
    serde_json::from_slice(&bytes).map_err(|e| Error::Io {
        path: path.to_path_buf(),
        source: std::io::Error::new(std::io::ErrorKind::InvalidData, e),
    })
}

fn verify_dedup(cfg: &Config, out: &Path, log_dir: &Path) -> secpipe::Result<bool> {
    let scratch_dir = out.join("dedup-check");
    std::fs::create_dir_all(&scratch_dir).map_err(|source| Error::Io {
        path: scratch_dir.clone(),
        source,
    })?;
    let scratch_db = scratch_dir.join("secpipe.db");
    if scratch_db.exists() {
        std::fs::remove_file(&scratch_db).map_err(|source| Error::Io {
            path: scratch_db.clone(),
            source,
        })?;
    }
    let scratch_ctx = RunContext::new(cfg.clone(), &scratch_dir);
    let _first = pipeline::ingest(&scratch_ctx, log_dir)?;
    let store_a = secpipe::store::Store::open(&scratch_ctx.db_path)?;
    let total_records = store_a.count_records()?;
    let total_quarantine = store_a.count_quarantine()?;
    drop(store_a);

    let second = pipeline::reingest_for_dedup(&scratch_ctx, log_dir)?;
    let store_b = secpipe::store::Store::open(&scratch_ctx.db_path)?;
    let stable = store_b.count_records()? == total_records
        && store_b.count_quarantine()? == total_quarantine
        && second.inserted == 0
        && second.duplicate == total_records;
    drop(store_b);
    Ok(stable)
}

fn produce_report(
    ctx: &RunContext,
    logs: &Path,
    out: &Path,
    run_self_check: bool,
) -> secpipe::Result<()> {
    let truth_path = logs.join("truth.json");
    let truth = load_truth(&truth_path)?;
    let correlation = pipeline::correlate(ctx)?;
    let pass = if run_self_check {
        pipeline::self_check(ctx, &correlation).is_ok()
    } else {
        true
    };
    let store = secpipe::store::Store::open(&ctx.db_path)?;
    let stats = pipeline::IngestStats {
        wraparounds: store.load_clock()?.map(|c| c.wraparounds).unwrap_or(0),
        late_events: store
            .get_meta("late_events")?
            .and_then(|v| v.parse().ok())
            .unwrap_or(0),
        restatements: store
            .get_meta("restatements")?
            .and_then(|v| v.parse().ok())
            .unwrap_or(0),
        ..Default::default()
    };
    let counters = pipeline::snapshot(&store, &stats)?;
    let metrics = pipeline::evaluate_metrics(&correlation, &truth, &counters, true, pass);
    report::write_artifacts(out, &metrics)?;
    println!("{}", metrics.summary_line());
    if run_self_check && !pass {
        return Err(Error::SelfCheck("self check failed".into()));
    }
    Ok(())
}

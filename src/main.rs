use clap::{Args, Parser, Subcommand};
use std::collections::BTreeMap;
use std::fs::{self, File};
use std::io::{BufWriter, Write};
use std::net::SocketAddr;
use std::path::{Path, PathBuf};
use std::process::ExitCode;
use std::time::{Duration, Instant};
use thiserror::Error;

#[path = "jitter/controller.rs"]
mod controller;
mod metrics;
mod model;
mod plc;
#[path = "jitter/reorder.rs"]
mod reorder;
#[path = "rx/trace.rs"]
mod trace;
#[path = "rx/udp.rs"]
mod udp;

use crate::controller::{Controller, ControllerConfig, TickOutput};
use crate::metrics::{MetricsReport, MetricsWriter, PacketCounts, TickRecord};
use crate::model::{pcm_to_hex, FRAME_SAMPLES, SAMPLE_RATE};
use crate::trace::TraceHeader;

#[derive(Parser, Debug)]
#[command(
    name = "jitter-relay",
    version,
    about = "Adaptive jitter-buffer relay demo"
)]
struct Cli {
    #[command(subcommand)]
    command: Command,
}

#[derive(Subcommand, Debug)]
enum Command {
    Gen(GenArgs),
    Replay(ReplayArgs),
    Serve(ServeArgs),
    Send(SendArgs),
    Demo(DemoArgs),
    Report(ReportArgs),
}

#[derive(Args, Debug)]
struct GenArgs {
    #[arg(long)]
    out: PathBuf,
    #[arg(long, default_value_t = 7)]
    seed: u64,
    #[arg(long, default_value_t = 30)]
    seconds: u64,
    #[arg(long, default_value_t = 0.06)]
    loss: f64,
    #[arg(long, default_value_t = 0.02)]
    dup: f64,
    #[arg(long, default_value_t = 0.08)]
    reorder: f64,
    #[arg(long, default_value_t = 35)]
    jitter_ms: u64,
    #[arg(long, default_value_t = 1)]
    ssrc: u32,
}

#[derive(Args, Debug)]
struct ReplayArgs {
    #[arg(long)]
    trace: PathBuf,
    #[arg(long, default_value = "out/")]
    out: PathBuf,
    #[arg(long)]
    json: Option<PathBuf>,
    #[arg(long, default_value_t = 0.8)]
    min_continuity: f64,
    #[arg(long, default_value_t = 12)]
    max_underruns: u64,
}

#[derive(Args, Debug)]
struct ServeArgs {
    #[arg(long, default_value = "127.0.0.1:9401")]
    bind: SocketAddr,
    #[arg(long, default_value = "out/serve.wav")]
    out: PathBuf,
    #[arg(long)]
    json: Option<PathBuf>,
    #[arg(long, default_value_t = 0)]
    max_frames: u64,
    #[arg(long = "frames-total", default_value_t = 0)]
    frames_total: u64,
}

#[derive(Args, Debug)]
struct SendArgs {
    #[arg(long)]
    trace: PathBuf,
    #[arg(long)]
    to: SocketAddr,
    #[arg(long, default_value_t = 1.0)]
    speed: f64,
}

#[derive(Args, Debug)]
struct DemoArgs {
    #[arg(long)]
    trace: PathBuf,
    #[arg(long, default_value = "127.0.0.1:9401")]
    bind: SocketAddr,
    #[arg(long)]
    json: Option<PathBuf>,
    #[arg(long)]
    compare: Option<PathBuf>,
    #[arg(long)]
    out: Option<PathBuf>,
}

#[derive(Args, Debug)]
struct ReportArgs {
    #[arg(long)]
    json: PathBuf,
    #[arg(long, default_value_t = 0.8)]
    min_continuity: f64,
    #[arg(long, default_value_t = 12)]
    max_underruns: u64,
}

fn main() -> ExitCode {
    let cli = Cli::parse();
    match run(cli.command) {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("error: {error}");
            ExitCode::from(error.exit_code())
        }
    }
}

fn run(command: Command) -> Result<(), AppError> {
    match command {
        Command::Gen(args) => run_gen(args),
        Command::Replay(args) => run_replay(args),
        Command::Serve(args) => run_serve(args),
        Command::Send(args) => run_send(args),
        Command::Demo(args) => run_demo(args),
        Command::Report(args) => run_report(args),
    }
}

fn run_gen(args: GenArgs) -> Result<(), AppError> {
    validate_gen(&args)?;
    let frames_total = args.seconds.saturating_mul(1_000 / model::FRAME_MS);
    let plan = build_plan(&args, frames_total);
    let packet_count: u64 = plan
        .iter()
        .map(|frame| {
            if frame.lost {
                0
            } else {
                1 + frame.duplicate as u64
            }
        })
        .sum();
    let source_losses = plan.iter().filter(|frame| frame.lost).count() as u64;
    let duplicates = packet_count.saturating_sub(frames_total - source_losses);
    let header = TraceHeader {
        trace_version: 1,
        frames_total,
        duration_ms: args.seconds * 1_000,
        sample_rate: SAMPLE_RATE,
        frame_ms: model::FRAME_MS,
        packet_count,
        seed: args.seed,
    };
    if let Some(parent) = args.out.parent() {
        if !parent.as_os_str().is_empty() {
            fs::create_dir_all(parent)?;
        }
    }
    let output = File::create(&args.out)?;
    let mut writer = BufWriter::new(output);
    writeln!(writer, "{}", serde_json::to_string(&header)?)?;
    let mut events: BTreeMap<(u64, u64, u16), model::Packet> = BTreeMap::new();
    let mut event_order = 0u64;
    for (seq, planned) in plan.iter().enumerate() {
        let source_ms = seq as u64 * model::FRAME_MS;
        if !planned.lost {
            let packet = model::Packet {
                ssrc: args.ssrc,
                seq: seq as u16,
                ts: seq as u64 * FRAME_SAMPLES as u64,
                arrival_us: planned.arrival_ms * 1_000,
                pcm: render_frame(seq),
            };
            events.insert(
                (planned.arrival_ms, event_order, packet.seq),
                packet.clone(),
            );
            event_order += 1;
            if planned.duplicate {
                let mut duplicate = packet;
                duplicate.arrival_us = planned.duplicate_arrival_ms * 1_000;
                events.insert(
                    (planned.duplicate_arrival_ms, event_order, duplicate.seq),
                    duplicate,
                );
                event_order += 1;
            }
        }
        let safe_before = source_ms.saturating_add(model::FRAME_MS + 5);
        while events.keys().next().is_some_and(|key| key.0 <= safe_before) {
            let (_, packet) = events.pop_first().expect("checked");
            write_packet(&mut writer, &packet)?;
        }
    }
    for (_, packet) in events {
        write_packet(&mut writer, &packet)?;
    }
    writer.flush()?;
    println!(
        "generated {} frames, {} packet records ({} losses, {} duplicates) -> {}",
        frames_total,
        packet_count,
        source_losses,
        duplicates,
        args.out.display()
    );
    Ok(())
}

fn run_replay(args: ReplayArgs) -> Result<(), AppError> {
    validate_gate(args.min_continuity, args.max_underruns)?;
    let (header, mut reader) = trace::TraceReader::open(&args.trace)?;
    let pcm_path = args.out.join("playback.wav");
    let mut metrics = MetricsWriter::create(&pcm_path, header.frames_total)?;
    let mut controller = Controller::new(ControllerConfig::default());
    controller.set_total_frames(header.frames_total);
    let mut clock = trace::VirtualClock::default();
    while let Some(frame) = reader.next_frame() {
        let frame = frame?;
        clock.advance_to(frame.arrival_ms())?;
        consume_frame(&mut controller, &mut metrics, frame)?;
    }
    reader.finish()?;
    let report = complete(controller, metrics, header.frames_total)?;
    let report_path = args.json.unwrap_or_else(|| args.out.join("report.json"));
    metrics::write_report(&report, &report_path)?;
    print_report(&report, &pcm_path, &report_path);
    gate_report(&report, args.min_continuity, args.max_underruns)
}

fn run_send(args: SendArgs) -> Result<(), AppError> {
    if !(args.speed > 0.0 && args.speed <= 1_000.0) {
        return Err(AppError::Config("speed must be in (0, 1000]".into()));
    }
    let (_, mut reader) = trace::TraceReader::open(&args.trace)?;
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()?;
    runtime.block_on(async {
        let socket = udp::bind_sender(args.to).await?;
        let started = Instant::now();
        let mut sent = 0u64;
        while let Some(frame) = reader.next_frame() {
            let frame = frame?;
            let target = Duration::from_secs_f64(
                (frame.arrival_ms() as f64 / 1_000.0 / args.speed).max(0.0),
            );
            if let Some(wait) = target.checked_sub(started.elapsed()) {
                tokio::time::sleep(wait).await;
            }
            let packet = frame.into_packet();
            udp::send_packet(&socket, args.to, &packet).await?;
            sent += 1;
        }
        reader.finish()?;
        println!("sent {sent} packets to {}", args.to);
        Ok::<(), AppError>(())
    })
}

fn run_serve(args: ServeArgs) -> Result<(), AppError> {
    let frames_total = if args.frames_total > 0 {
        args.frames_total
    } else {
        args.max_frames
    };
    if frames_total == 0 {
        return Err(AppError::Config(
            "serve needs --frames-total or --max-frames to close the report".into(),
        ));
    }
    let receive_target = if args.max_frames > 0 {
        args.max_frames.min(frames_total)
    } else {
        frames_total
    };
    let pcm_path = args.out;
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()?;
    runtime.block_on(async {
        let mut receiver = udp::UdpReceiver::bind(args.bind, udp::TimingMode::Monotonic).await?;
        let mut metrics = MetricsWriter::create(&pcm_path, frames_total)?;
        let mut controller = Controller::new(ControllerConfig::default());
        controller.set_total_frames(frames_total);
        let mut received = 0u64;
        while received < receive_target {
            match receiver.recv_timeout().await? {
                Some(frame) => {
                    consume_frame(&mut controller, &mut metrics, frame)?;
                    received += 1;
                }
                None => break,
            }
        }
        let report = complete(controller, metrics, frames_total)?;
        if let Some(path) = args.json {
            metrics::write_report(&report, &path)?;
            println!("report={}", path.display());
        }
        print_report(&report, &pcm_path, Path::new(""));
        println!("received {received} UDP packets on {}", args.bind);
        Ok::<(), AppError>(())
    })
}

fn run_demo(args: DemoArgs) -> Result<(), AppError> {
    let (header, mut reader) = trace::TraceReader::open(&args.trace)?;
    let report_path = args
        .json
        .clone()
        .unwrap_or_else(|| PathBuf::from("out/demo.json"));
    let pcm_path = args.out.clone().unwrap_or_else(|| {
        report_path
            .parent()
            .unwrap_or_else(|| Path::new("."))
            .join("demo.wav")
    });
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()?;
    let report = runtime.block_on(async {
        let mut receiver = udp::UdpReceiver::bind(
            args.bind,
            udp::TimingMode::Embedded { start_us: 0 },
        )
        .await?;
        let target = receiver.local_addr()?;
        let sender_socket = udp::bind_sender(target).await?;
        let mut metrics = MetricsWriter::create(&pcm_path, header.frames_total)?;
        let mut controller = Controller::new(ControllerConfig::default());
        controller.set_total_frames(header.frames_total);
        let expected_packets = header.packet_count;
        let sender = tokio::spawn(async move {
            let mut sent = 0u64;
            while let Some(frame) = reader.next_frame() {
                let packet = frame?.into_packet();
                udp::send_packet(&sender_socket, target, &packet).await?;
                sent += 1;
                if sent % 32 == 0 {
                    tokio::time::sleep(Duration::from_millis(1)).await;
                }
            }
            reader.finish()?;
            Ok::<u64, AppError>(sent)
        });
        let mut received = 0u64;
        while received < expected_packets {
            let frame = receiver.recv_timeout().await?.ok_or_else(|| {
                AppError::Gate(format!(
                    "loopback UDP timed out after {received}/{expected_packets} packets"
                ))
            })?;
            consume_frame(&mut controller, &mut metrics, frame)?;
            received += 1;
        }
        let sent = sender
            .await
            .map_err(|error| std::io::Error::other(format!("UDP sender task: {error}")))??;
        if sent != expected_packets || received != expected_packets {
            return Err(AppError::Gate(format!(
                "loopback packet count mismatch: sent={sent} received={received} expected={expected_packets}"
            )));
        }
        complete(controller, metrics, header.frames_total)
    })?;
    metrics::write_report(&report, &report_path)?;
    print_report(&report, &pcm_path, &report_path);
    if let Some(compare_path) = args.compare {
        let baseline = metrics::read_report(&compare_path)?;
        if baseline != report {
            return Err(AppError::Gate(format!(
                "demo and replay metrics differ (compare {})",
                compare_path.display()
            )));
        }
        println!("demo/replay metric diff = 0");
    }
    gate_report(&report, 0.8, 12)
}

fn consume_frame(
    controller: &mut Controller,
    metrics: &mut MetricsWriter,
    frame: model::Frame,
) -> Result<(), AppError> {
    let ticks = controller.advance_to(frame.arrival_ms());
    record_ticks(metrics, &ticks)?;
    controller.push(frame);
    Ok(())
}

fn record_ticks(metrics: &mut MetricsWriter, ticks: &[TickOutput]) -> Result<(), AppError> {
    for tick in ticks {
        metrics.record(TickRecord {
            time_ms: tick.time_ms,
            water_ms: tick.water_ms,
            target_ms: tick.target_ms,
            arrivals: tick.arrivals,
            late: tick.late,
            duplicate: tick.duplicate,
            concealed: tick.concealed,
            concealed_frame: tick.concealed_frame,
            underrun_started: tick.underrun_started,
            frame: tick.frame.as_ref(),
        })?;
    }
    Ok(())
}

fn complete(
    mut controller: Controller,
    mut metrics: MetricsWriter,
    frames_total: u64,
) -> Result<MetricsReport, AppError> {
    let ticks = controller.finish(frames_total);
    record_ticks(&mut metrics, &ticks)?;
    let stats = controller.stats();
    let counts = PacketCounts {
        packets_ok: stats.arrivals.saturating_add(stats.late),
        dup_dropped: stats.duplicate,
        late_dropped: stats.late,
        conflicts: stats.conflict,
        overruns: stats.overrun,
        resets: stats.resets,
    };
    Ok(metrics.finish(
        counts,
        controller.buffer_max_ms(),
        controller.target_max_ms(),
    )?)
}

fn run_report(args: ReportArgs) -> Result<(), AppError> {
    validate_gate(args.min_continuity, args.max_underruns)?;
    let report = metrics::read_report(&args.json)?;
    print_report(&report, Path::new(""), &args.json);
    gate_report(&report, args.min_continuity, args.max_underruns)
}

fn print_report(report: &MetricsReport, pcm_path: &Path, report_path: &Path) {
    let mut levels: Vec<u64> = report.water.iter().map(|sample| sample.water_ms).collect();
    levels.sort_unstable();
    let p50 = percentile(&levels, 0.50);
    let p95 = percentile(&levels, 0.95);
    println!(
        "frames={} played={} ok={} dup={} late={} conflicts={} overruns={} resets={}",
        report.frames_total,
        report.frames_played,
        report.packets_ok,
        report.dup_dropped,
        report.late_dropped,
        report.conflicts,
        report.overruns,
        report.resets
    );
    println!(
        "water p50={}ms p95={}ms max={}ms target_max={}ms samples={}",
        p50,
        p95,
        report.buffer_max_ms,
        report.target_max_ms,
        report.water.len()
    );
    println!(
        "concealed={} longest_run={} underruns={} energy_jumps={} continuity={:.4}",
        report.concealed_frames,
        report.longest_conceal_run,
        report.underruns,
        report.energy_jumps,
        report.continuity_score
    );
    if !pcm_path.as_os_str().is_empty() {
        println!(
            "pcm={} report={}",
            pcm_path.display(),
            report_path.display()
        );
    }
}

fn percentile(sorted: &[u64], fraction: f64) -> u64 {
    if sorted.is_empty() {
        return 0;
    }
    let index = ((sorted.len() - 1) as f64 * fraction).round() as usize;
    sorted[index.min(sorted.len() - 1)]
}

fn validate_gate(min_continuity: f64, _max_underruns: u64) -> Result<(), AppError> {
    if !(0.0..=1.0).contains(&min_continuity) {
        return Err(AppError::Config("min-continuity must be in 0..=1".into()));
    }
    Ok(())
}

fn gate_report(
    report: &MetricsReport,
    min_continuity: f64,
    max_underruns: u64,
) -> Result<(), AppError> {
    let mut failures = Vec::new();
    if report.continuity_score < min_continuity {
        failures.push(format!(
            "continuity {:.4} < {:.4}",
            report.continuity_score, min_continuity
        ));
    }
    if report.underruns > max_underruns {
        failures.push(format!(
            "underruns {} > {}",
            report.underruns, max_underruns
        ));
    }
    if failures.is_empty() {
        Ok(())
    } else {
        Err(AppError::Gate(failures.join("; ")))
    }
}

fn write_packet(writer: &mut BufWriter<File>, packet: &model::Packet) -> Result<(), AppError> {
    writeln!(
        writer,
        "{{\"arrival_ms\":{},\"ssrc\":{},\"seq\":{},\"ts\":{},\"pcm\":\"{}\"}}",
        packet.arrival_us / 1_000,
        packet.ssrc,
        packet.seq,
        packet.ts,
        pcm_to_hex(&packet.pcm)
    )?;
    Ok(())
}

#[derive(Clone, Debug)]
struct FramePlan {
    lost: bool,
    duplicate: bool,
    arrival_ms: u64,
    duplicate_arrival_ms: u64,
}

fn build_plan(args: &GenArgs, frames_total: u64) -> Vec<FramePlan> {
    let mut rng = SplitMix64::new(args.seed ^ 0xa5a5_5a5a_dead_beef);
    let total = frames_total as usize;
    let loss_count = (args.loss * frames_total as f64).round() as usize;
    let duplicate_count = (args.dup * frames_total as f64).round() as usize;
    let reorder_count = (args.reorder * frames_total as f64).round() as usize;
    let mut lost = vec![false; total];
    let burst = loss_count.min(5);
    if burst > 0 {
        let start = rng.below((total - burst).max(1) as u64) as usize;
        lost[start..start + burst].fill(true);
    }
    let remaining_loss = loss_count.saturating_sub(burst);
    let loss_candidates: Vec<usize> = (0..total).filter(|index| !lost[*index]).collect();
    for index in choose_positions(&loss_candidates, remaining_loss, &mut rng) {
        lost[index] = true;
    }

    let live: Vec<usize> = (0..total).filter(|index| !lost[*index]).collect();
    let mut duplicate = vec![false; total];
    for index in choose_positions(&live, duplicate_count, &mut rng) {
        duplicate[index] = true;
    }
    let mut reordered = vec![false; total];
    for index in choose_positions(&live, reorder_count, &mut rng) {
        reordered[index] = true;
    }
    let late_target = (reorder_count as f64 * 0.17).round() as usize;
    let late_candidates: Vec<usize> = live
        .iter()
        .copied()
        .filter(|index| reordered[*index] && !near_loss(*index, &lost))
        .collect();
    let mut late = vec![false; total];
    for index in choose_positions(&late_candidates, late_target, &mut rng) {
        late[index] = true;
    }

    let mut plan = Vec::with_capacity(total);
    let jitter_span = (args.jitter_ms.saturating_mul(5)).clamp(30, 200);
    for seq in 0..frames_total {
        let source_ms = seq * model::FRAME_MS;
        let mut arrival_ms = source_ms + 5 + rng.below(jitter_span + 1);
        let index = seq as usize;
        if reordered[index] {
            arrival_ms += 30 + rng.below(91);
        }
        if late[index] {
            arrival_ms = source_ms + 320 + rng.below(61);
        }
        let duplicate_arrival_ms = arrival_ms + 5 + rng.below(31);
        plan.push(FramePlan {
            lost: lost[index],
            duplicate: duplicate[index],
            arrival_ms,
            duplicate_arrival_ms,
        });
    }
    plan
}

fn choose_positions(candidates: &[usize], count: usize, rng: &mut SplitMix64) -> Vec<usize> {
    let mut ranked: Vec<(u64, usize)> = candidates
        .iter()
        .map(|index| (rng.next_u64(), *index))
        .collect();
    ranked.sort_unstable_by_key(|entry| entry.0);
    ranked
        .into_iter()
        .take(count.min(candidates.len()))
        .map(|entry| entry.1)
        .collect()
}

fn near_loss(index: usize, lost: &[bool]) -> bool {
    let start = index.saturating_sub(2);
    let end = (index + 2).min(lost.len().saturating_sub(1));
    lost[start..=end].iter().any(|value| *value)
}

fn render_frame(seq: usize) -> Vec<i16> {
    let frame_index = seq as u64 * FRAME_SAMPLES as u64;
    let mut out = Vec::with_capacity(FRAME_SAMPLES);
    for index in 0..FRAME_SAMPLES {
        let t = (frame_index + index as u64) as f64 / SAMPLE_RATE as f64;
        let sample = (t * 2.0 * std::f64::consts::PI * 220.0).sin() * 8_000.0
            + (t * 2.0 * std::f64::consts::PI * 440.0).sin() * 2_500.0;
        out.push(sample.round() as i16);
    }
    out
}

fn validate_gen(args: &GenArgs) -> Result<(), AppError> {
    if args.seconds == 0 || args.seconds > 3_600 {
        return Err(AppError::Config("seconds must be in 1..=3600".into()));
    }
    for (name, value) in [
        ("loss", args.loss),
        ("dup", args.dup),
        ("reorder", args.reorder),
    ] {
        if !(0.0..=1.0).contains(&value) {
            return Err(AppError::Config(format!("{name} must be in 0..=1")));
        }
    }
    if args.jitter_ms > 1_000 {
        return Err(AppError::Config("jitter-ms must be <= 1000".into()));
    }
    Ok(())
}

struct SplitMix64 {
    state: u64,
}

impl SplitMix64 {
    fn new(seed: u64) -> Self {
        Self { state: seed }
    }

    fn next_u64(&mut self) -> u64 {
        self.state = self.state.wrapping_add(0x9e37_79b9_7f4a_7c15);
        let mut value = self.state;
        value = (value ^ (value >> 30)).wrapping_mul(0xbf58_476d_1ce4_e5b9);
        value = (value ^ (value >> 27)).wrapping_mul(0x94d0_49bb_1331_11eb);
        value ^ (value >> 31)
    }

    fn below(&mut self, upper_exclusive: u64) -> u64 {
        if upper_exclusive == 0 {
            0
        } else {
            self.next_u64() % upper_exclusive
        }
    }
}

#[derive(Debug, Error)]
enum AppError {
    #[error("invalid parameters: {0}")]
    Config(String),
    #[error("{0}")]
    Gate(String),
    #[error("trace error: {0}")]
    Trace(#[from] trace::TraceError),
    #[error("metrics error: {0}")]
    Metrics(#[from] metrics::MetricsError),
    #[error("UDP error: {0}")]
    Udp(#[from] udp::UdpError),
    #[error("I/O error: {0}")]
    Io(#[from] std::io::Error),
    #[error("JSON error: {0}")]
    Json(#[from] serde_json::Error),
}

impl AppError {
    fn exit_code(&self) -> u8 {
        match self {
            Self::Config(_) => 2,
            Self::Gate(_) => 4,
            Self::Trace(trace::TraceError::Io(_)) => 5,
            Self::Trace(_) => 3,
            Self::Metrics(_) | Self::Io(_) => 5,
            Self::Json(_) => 5,
            Self::Udp(error) => match error {
                udp::UdpError::ForbiddenAddress(_) => 2,
                udp::UdpError::Packet(_) => 3,
                udp::UdpError::Io(_) | udp::UdpError::TimestampRollback { .. } => 5,
            },
        }
    }
}

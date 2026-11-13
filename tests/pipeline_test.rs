use std::path::PathBuf;
use std::time::{SystemTime, UNIX_EPOCH};

use soc_correlator::gen;
use soc_correlator::pipeline;
use soc_correlator::store::Store;
use soc_correlator::timewin::{TimeNormalizer, CLOCK32_MODULUS};

fn temporary_directory(name: &str) -> PathBuf {
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .expect("clock after epoch")
        .as_nanos();
    std::env::temp_dir().join(format!(
        "soc-correlator-{name}-{}-{nanos}",
        std::process::id()
    ))
}

#[test]
fn wrap_regression_adds_exactly_one_modulus_per_reset() {
    let wall = 1_700_000_000_i64;
    let raw0 = CLOCK32_MODULUS - 7;
    let anchor = wall - raw0;
    let mut normalizer = TimeNormalizer::new();

    assert_eq!(
        normalizer
            .normalize("legacy", CLOCK32_MODULUS - 3, 32, Some(anchor))
            .unwrap(),
        wall + 4
    );
    assert_eq!(
        normalizer.normalize("legacy", 2, 32, Some(anchor)).unwrap(),
        wall + 9
    );
    assert_eq!(normalizer.wrap_notices().len(), 1);
}

#[test]
fn end_to_end_ingest_is_idempotent() {
    let root = temporary_directory("pipeline");
    let fixtures = root.join("fixtures");
    let database = root.join("security.db");
    let manifest = gen::generate(42, &fixtures, 24_000).expect("generate fixture");

    let first = pipeline::ingest(&fixtures, &database, false).expect("first ingest");
    assert_eq!(first.accepted_parsed, manifest.accepted_lines);
    assert_eq!(first.rejected_parsed, manifest.malformed_lines);
    assert_eq!(first.new_wraps, 1);
    assert_eq!(first.new_identity_conflicts, 7);
    assert_eq!(first.late_dropped, manifest.late_injected);

    let before = {
        let store = Store::open(&database).expect("open store");
        store.stats().expect("stats")
    };
    assert_eq!(before.parse_success, manifest.accepted_lines as u64);
    assert_eq!(before.parse_rejected, manifest.malformed_lines as u64);
    assert_eq!(before.windows, 48);
    assert_eq!(before.closed_windows, 48);
    assert_eq!(before.late_dropped, manifest.late_injected as u64);

    let second = pipeline::ingest(&fixtures, &database, false).expect("second ingest");
    assert_eq!(second.inserted_events, 0);
    assert_eq!(second.duplicate_events, manifest.accepted_lines);
    assert_eq!(second.late_dropped, 0);
    assert_eq!(second.new_wraps, 0);
    assert_eq!(second.new_identity_conflicts, 0);

    let after = {
        let store = Store::open(&database).expect("open store");
        store.stats().expect("stats")
    };
    assert_eq!(after.stored_events, before.stored_events);
    assert_eq!(after.parse_success, before.parse_success);
    assert_eq!(after.parse_rejected, before.parse_rejected);
    assert_eq!(after.late_dropped, before.late_dropped);
    assert_eq!(after.wraps, before.wraps);
    assert_eq!(after.identity_conflicts, before.identity_conflicts);

    let reopened = pipeline::ingest(&fixtures, &database, true).expect("forced reopen");
    assert_eq!(reopened.reopened_windows, 48);
    assert_eq!(reopened.inserted_events, manifest.late_injected);
    assert_eq!(reopened.late_dropped, 0);
    assert_eq!(
        reopened.duplicate_events,
        manifest.accepted_lines - manifest.late_injected
    );

    let after_reopen = {
        let store = Store::open(&database).expect("open store");
        store.stats().expect("stats")
    };
    assert_eq!(after_reopen.parse_success, before.parse_success);
    assert_eq!(after_reopen.late_dropped, 0);
    assert_eq!(
        after_reopen.stored_events,
        before.stored_events + manifest.late_injected as u64
    );
    assert_eq!(after_reopen.closed_windows, 48);

    let _ = std::fs::remove_dir_all(root);
}

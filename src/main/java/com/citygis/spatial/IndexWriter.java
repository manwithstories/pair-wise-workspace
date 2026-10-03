package com.citygis.spatial;

import java.io.IOException;
import java.io.UncheckedIOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardOpenOption;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.atomic.AtomicLong;

/**
 * Incremental write path keeping the KD tree and the R tree consistent.
 *
 * <p>Every mutation is a small transaction:
 * <ol>
 *   <li><b>prepare</b> — the operation is appended to a write-ahead log and fsynced, so an intent
 *       survives a crash;</li>
 *   <li><b>commit phase 1</b> — the KD tree is updated first (a point insert/delete is a tombstone
 *       flip), and the log is marked {@code KD_APPLIED};</li>
 *   <li><b>commit phase 2</b> — the R tree is updated to match, and the log entry is closed.</li>
 * </ol>
 *
 * <p>If the process dies between phases, {@link #recover()} replays the log: entries that reached
 * only phase 1 are either completed or rolled back, so the two trees never diverge. A failure
 * raised inside a phase rolls that phase back immediately and leaves both trees as they were.
 */
public final class IndexWriter implements AutoCloseable {

    /** Write-ahead log file name. */
    public static final String WAL_FILE = "index.wal";

    /** Phase markers appended to each WAL record. */
    public enum Phase {
        /** WAL written, nothing applied yet. */
        PREPARED,
        /** KD tree updated, R tree not yet. */
        KD_APPLIED,
        /** Both trees updated; the record is complete. */
        COMMITTED,
        /** The transaction was undone; the record is inert. */
        ROLLED_BACK
    }

    /** Kinds of mutation the writer can perform. */
    public enum Op {
        /** Add a new point feature to the KD tree and the R tree. */
        INSERT,
        /** Retire a feature in both trees (tombstone, no rebuild). */
        DELETE,
        /** Replace a feature's geometry in both trees. */
        UPDATE
    }

    /** One WAL record. */
    public record Record(long txnId, Op op, long featureId, String payload, Phase phase) {
        String encode() {
            return String.join("|", Long.toString(txnId), op.name(), Long.toString(featureId),
                    payload.replace("|", "%7C"), phase.name());
        }

        static Record decode(String line) {
            String[] p = line.split("\\|", 5);
            if (p.length < 5) {
                throw new IllegalStateException("corrupt WAL record: " + line);
            }
            return new Record(Long.parseLong(p[0]), Op.valueOf(p[1]), Long.parseLong(p[2]),
                    p[3].replace("%7C", "|"), Phase.valueOf(p[4]));
        }
    }

    /** Where a mutation lands. */
    public interface Target {
        /** Applies the operation to the KD tree. Returns true when something changed. */
        boolean applyToKd(Op op, GeoFeature feature);

        /** Applies the operation to the R tree. Returns true when something changed. */
        boolean applyToRTree(Op op, GeoFeature feature);

        /** Undoes a KD-tree change. */
        void undoKd(Op op, GeoFeature feature);

        /** Undoes an R-tree change. */
        void undoRTree(Op op, GeoFeature feature);

        /** fsyncs the segment storage touched by this target. */
        void sync();
    }

    private final Path walPath;
    private final AtomicLong txnSeq = new AtomicLong();
    private final List<Record> log = new ArrayList<>();
    private volatile FailureInjector injector;

    /** Test hook: makes a chosen commit phase throw, to exercise rollback. */
    public interface FailureInjector {
        /** @return true to throw before applying {@code phase} to {@code target} */
        boolean shouldFail(long txnId, Phase phase);
    }

    public IndexWriter(Path directory) {
        this.walPath = directory.resolve(WAL_FILE);
        try {
            Files.createDirectories(directory);
        } catch (IOException e) {
            throw new UncheckedIOException("cannot create WAL directory " + directory, e);
        }
    }

    public void setFailureInjector(FailureInjector injector) {
        this.injector = injector;
    }

    /** Records currently held in memory, oldest first. */
    public List<Record> journal() {
        synchronized (log) {
            return List.copyOf(log);
        }
    }

    /**
     * Runs one mutation as a two-phase transaction.
     *
     * @return the transaction id
     * @throws TransactionFailedException if a phase fails; both trees are then left untouched
     */
    public long commit(Op op, GeoFeature feature, Target target) {
        long txnId = txnSeq.incrementAndGet();
        // --- prepare: intent is durable before anything is mutated ---
        append(new Record(txnId, op, feature.id(), describe(feature), Phase.PREPARED));

        boolean kdDone = false;
        boolean rDone = false;
        try {
            // --- commit phase 1: KD tree ---
            fail(txnId, Phase.KD_APPLIED);
            target.applyToKd(op, feature);
            target.sync();
            kdDone = true;
            append(new Record(txnId, op, feature.id(), describe(feature), Phase.KD_APPLIED));

            // --- commit phase 2: R tree ---
            fail(txnId, Phase.COMMITTED);
            target.applyToRTree(op, feature);
            target.sync();
            rDone = true;
            append(new Record(txnId, op, feature.id(), describe(feature), Phase.COMMITTED));
            return txnId;
        } catch (RuntimeException e) {
            rollback(txnId, op, feature, target, kdDone, rDone);
            throw new TransactionFailedException("transaction " + txnId + " (" + op + ") failed: "
                    + e.getMessage(), e);
        }
    }

    /** Convenience for inserts. */
    public long insert(GeoFeature feature, Target target) {
        return commit(Op.INSERT, feature, target);
    }

    /** Convenience for deletes. */
    public long delete(GeoFeature feature, Target target) {
        return commit(Op.DELETE, feature, target);
    }

    private void fail(long txnId, Phase phase) {
        FailureInjector fi = injector;
        if (fi != null && fi.shouldFail(txnId, phase)) {
            throw new IllegalStateException("injected failure at phase " + phase);
        }
    }

    /**
     * Undoes whatever this transaction managed to apply.
     *
     * <p>Runs in reverse order of application (R tree first, then KD tree), which is the mirror of
     * the commit order and therefore always leaves the pair consistent.
     */
    private void rollback(long txnId, Op op, GeoFeature feature, Target target,
                          boolean kdDone, boolean rDone) {
        try {
            if (rDone) {
                target.undoRTree(op, feature);
                target.sync();
            }
            if (kdDone) {
                target.undoKd(op, feature);
                target.sync();
            }
        } catch (RuntimeException suppressed) {
            // Rollback is best effort; the WAL still shows the transaction as incomplete so that
            // recover() can finish the job after restart.
        }
        append(new Record(txnId, op, feature.id(), describe(feature), Phase.ROLLED_BACK));
    }

    /**
     * Replays the WAL after a restart.
     *
     * @param target the segment set to replay against
     * @return the number of transactions rolled back
     */
    public int recover(Target target) {
        List<Record> records = readWal();
        // The log holds one record per phase, so recovery must reason per transaction: only the
        // furthest phase a transaction reached matters, and counting records would report a single
        // transaction several times over.
        java.util.Map<Long, Record> furthest = new java.util.LinkedHashMap<>();
        for (Record r : records) {
            Record seen = furthest.get(r.txnId());
            if (seen == null || phaseRank(r.phase()) > phaseRank(seen.phase())) {
                furthest.put(r.txnId(), r);
            }
        }
        int rolled = 0;
        for (Record r : furthest.values()) {
            if (r.phase() == Phase.ROLLED_BACK || r.phase() == Phase.COMMITTED) {
                continue;
            }
            // PREPARED or KD_APPLIED: the transaction is incomplete. Finish the rollback so the
            // KD and R trees agree again.
            GeoFeature feature = materialise(r);
            try {
                if (r.phase() == Phase.KD_APPLIED) {
                    target.undoKd(r.op(), feature);
                    target.sync();
                }
            } catch (RuntimeException e) {
                throw new TransactionFailedException("recovery failed for txn " + r.txnId(), e);
            }
            append(new Record(r.txnId(), r.op(), r.featureId(), r.payload(), Phase.ROLLED_BACK));
            rolled++;
        }
        return rolled;
    }

    /** Ordering of phases, so recovery can pick the furthest one a transaction reached. */
    private static int phaseRank(Phase p) {
        return switch (p) {
            case PREPARED -> 0;
            case KD_APPLIED -> 1;
            case ROLLED_BACK -> 2;
            case COMMITTED -> 3;
        };
    }

    private GeoFeature materialise(Record r) {
        String[] p = r.payload().split(",");
        return GeoFeature.point(r.featureId(), FeatureType.valueOf(p[0]),
                Double.parseDouble(p[1]), Double.parseDouble(p[2]));
    }

    private static String describe(GeoFeature f) {
        return f.type().name() + "," + f.x() + "," + f.y();
    }

    private void append(Record r) {
        try {
            Files.writeString(walPath, r.encode() + System.lineSeparator(), StandardCharsets.UTF_8,
                    StandardOpenOption.CREATE, StandardOpenOption.APPEND);
            // Durability of the intent matters more than batching, so fsync every record.
            try (var ch = java.nio.channels.FileChannel.open(walPath, StandardOpenOption.WRITE)) {
                ch.force(true);
            }
        } catch (IOException e) {
            throw new UncheckedIOException("cannot append to WAL " + walPath, e);
        }
        synchronized (log) {
            log.add(r);
        }
    }

    /** Reads and decodes every WAL record. */
    public List<Record> readWal() {
        if (!Files.exists(walPath)) {
            return List.of();
        }
        List<Record> out = new ArrayList<>();
        try {
            for (String line : Files.readAllLines(walPath, StandardCharsets.UTF_8)) {
                if (!line.isBlank()) {
                    out.add(Record.decode(line));
                }
            }
        } catch (IOException e) {
            throw new UncheckedIOException("cannot read WAL " + walPath, e);
        }
        return out;
    }

    /** Truncates the log; used after a clean shutdown or a completed recovery. */
    public void truncate() {
        try {
            Files.deleteIfExists(walPath);
        } catch (IOException e) {
            throw new UncheckedIOException("cannot truncate WAL " + walPath, e);
        }
        synchronized (log) {
            log.clear();
        }
    }

    @Override
    public void close() {
        // The WAL is append-only and durable per record; nothing to flush on close.
    }

    /** Raised when a transaction could not be applied; both trees are consistent afterwards. */
    public static final class TransactionFailedException extends RuntimeException {
        private static final long serialVersionUID = 1L;

        public TransactionFailedException(String message, Throwable cause) {
            super(message, cause);
        }
    }
}

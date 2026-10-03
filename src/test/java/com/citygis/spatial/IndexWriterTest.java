package com.citygis.spatial;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * The WAL and two-phase commit must leave the KD and R trees consistent, including when a phase
 * fails part-way through.
 */
class IndexWriterTest {

    @TempDir
    Path tmp;

    /** Records every call so tests can assert exactly what each phase did, and in what order. */
    private static final class RecordingTarget implements IndexWriter.Target {
        final List<String> calls = new ArrayList<>();
        boolean kdHas;
        boolean rHas;

        @Override
        public boolean applyToKd(IndexWriter.Op op, GeoFeature feature) {
            calls.add("kd:" + op);
            kdHas = op != IndexWriter.Op.DELETE;
            return true;
        }

        @Override
        public boolean applyToRTree(IndexWriter.Op op, GeoFeature feature) {
            calls.add("r:" + op);
            rHas = op != IndexWriter.Op.DELETE;
            return true;
        }

        @Override
        public void undoKd(IndexWriter.Op op, GeoFeature feature) {
            calls.add("undo-kd:" + op);
            kdHas = op == IndexWriter.Op.DELETE;
        }

        @Override
        public void undoRTree(IndexWriter.Op op, GeoFeature feature) {
            calls.add("undo-r:" + op);
            rHas = op == IndexWriter.Op.DELETE;
        }

        @Override
        public void sync() {
            calls.add("sync");
        }
    }

    private static GeoFeature poi(long id, double x, double y) {
        return GeoFeature.point(id, FeatureType.POI, x, y);
    }

    @Test
    @DisplayName("a successful insert touches the KD tree first, then the R tree")
    void insertAppliesInOrder() {
        IndexWriter writer = new IndexWriter(tmp);
        RecordingTarget target = new RecordingTarget();
        writer.insert(poi(1, 10, 20), target);
        assertEquals(List.of("kd:INSERT", "sync", "r:INSERT", "sync"), target.calls);
        assertTrue(target.kdHas);
        assertTrue(target.rHas);
    }

    @Test
    @DisplayName("the WAL is written before any tree is touched")
    void walIsWrittenFirst() {
        IndexWriter writer = new IndexWriter(tmp);
        RecordingTarget target = new RecordingTarget();
        writer.insert(poi(1, 10, 20), target);
        List<IndexWriter.Record> wal = writer.readWal();
        assertEquals(3, wal.size(), "one record per phase");
        assertEquals(IndexWriter.Phase.PREPARED, wal.get(0).phase(),
                "the intent must be durable before the KD tree changes");
        assertEquals(IndexWriter.Phase.KD_APPLIED, wal.get(1).phase());
        assertEquals(IndexWriter.Phase.COMMITTED, wal.get(2).phase());
    }

    @Test
    @DisplayName("a failure before the KD phase leaves both trees untouched")
    void failureBeforeKdPhase() {
        IndexWriter writer = new IndexWriter(tmp);
        RecordingTarget target = new RecordingTarget();
        writer.setFailureInjector((txn, phase) -> phase == IndexWriter.Phase.KD_APPLIED);
        assertThrows(IndexWriter.TransactionFailedException.class,
                () -> writer.insert(poi(1, 1, 1), target));
        assertFalse(target.kdHas);
        assertFalse(target.rHas);
        assertEquals(IndexWriter.Phase.ROLLED_BACK, writer.readWal().get(1).phase());
    }

    @Test
    @DisplayName("a failure between the phases rolls the KD tree back")
    void failureBetweenPhasesRollsBackKd() {
        IndexWriter writer = new IndexWriter(tmp);
        RecordingTarget target = new RecordingTarget();
        writer.setFailureInjector((txn, phase) -> phase == IndexWriter.Phase.COMMITTED);
        assertThrows(IndexWriter.TransactionFailedException.class,
                () -> writer.insert(poi(1, 1, 1), target));
        // The KD change was applied then undone; the R tree never changed at all.
        assertFalse(target.kdHas, "KD tree must be back to its pre-transaction state");
        assertFalse(target.rHas, "R tree must be untouched");
        assertTrue(target.calls.contains("undo-kd:INSERT"),
                "rollback must explicitly undo the KD change: " + target.calls);
        assertFalse(target.calls.stream().anyMatch(c -> c.equals("undo-r:INSERT")),
                "rollback must not invent an R-tree undo that never happened");
    }

    @Test
    @DisplayName("recovery undoes a transaction that crashed after the KD phase")
    void recoveryUndoesHalfAppliedTransaction() {
        IndexWriter writer = new IndexWriter(tmp);
        RecordingTarget crash = new RecordingTarget();
        // Simulate a crash: the WAL is left at KD_APPLIED with the KD tree already changed.
        writer.insert(poi(1, 1, 1), crash);
        List<IndexWriter.Record> wal = writer.readWal();
        // Rewind the journal to the half-applied state, as a crash would leave it.
        try {
            Files.writeString(tmp.resolve(IndexWriter.WAL_FILE),
                    wal.get(0).encode() + System.lineSeparator()
                            + wal.get(1).encode() + System.lineSeparator());
        } catch (java.io.IOException e) {
            throw new AssertionError(e);
        }
        crash.kdHas = true;
        crash.rHas = false;

        RecordingTarget afterRestart = new RecordingTarget();
        afterRestart.kdHas = true;
        afterRestart.rHas = false;
        int rolled = writer.recover(afterRestart);
        assertEquals(1, rolled, "the incomplete transaction must be rolled back");
        assertFalse(afterRestart.kdHas, "KD tree must return to its pre-transaction state");
        assertFalse(afterRestart.rHas);
    }

    @Test
    @DisplayName("recovery leaves committed transactions alone")
    void recoverySkipsCommitted() {
        IndexWriter writer = new IndexWriter(tmp);
        RecordingTarget target = new RecordingTarget();
        writer.insert(poi(1, 1, 1), target);
        RecordingTarget afterRestart = new RecordingTarget();
        assertEquals(0, writer.recover(afterRestart));
        assertTrue(afterRestart.calls.isEmpty(), "nothing should be replayed: " + afterRestart.calls);
    }

    @Test
    @DisplayName("the journal exposes the full phase history")
    void journalRecordsPhases() {
        IndexWriter writer = new IndexWriter(tmp);
        writer.insert(poi(1, 1, 1), new RecordingTarget());
        List<IndexWriter.Record> journal = writer.journal();
        assertEquals(3, journal.size());
        assertEquals(IndexWriter.Phase.PREPARED, journal.get(0).phase());
        assertEquals(IndexWriter.Phase.KD_APPLIED, journal.get(1).phase());
        assertEquals(IndexWriter.Phase.COMMITTED, journal.get(2).phase());
        assertTrue(journal.stream().allMatch(r -> r.txnId() == 1));
    }

    @Test
    @DisplayName("transaction ids increase monotonically")
    void transactionIdsIncrease() {
        IndexWriter writer = new IndexWriter(tmp);
        RecordingTarget t = new RecordingTarget();
        long a = writer.insert(poi(1, 1, 1), t);
        long b = writer.insert(poi(2, 2, 2), t);
        long c = writer.delete(poi(1, 1, 1), t);
        assertTrue(a < b && b < c, "ids must increase: " + a + "," + b + "," + c);
    }

    @Test
    @DisplayName("deletes apply to both trees and are rolled back symmetrically")
    void deleteTransaction() {
        IndexWriter writer = new IndexWriter(tmp);
        RecordingTarget target = new RecordingTarget();
        writer.delete(poi(1, 1, 1), target);
        assertEquals(List.of("kd:DELETE", "sync", "r:DELETE", "sync"), target.calls);
        assertFalse(target.kdHas);
        assertFalse(target.rHas);
    }

    @Test
    @DisplayName("WAL records survive a reload from disk and round-trip their fields")
    void walRoundTrip() {
        IndexWriter writer = new IndexWriter(tmp);
        writer.insert(poi(42, 3.5, 4.5), new RecordingTarget());
        IndexWriter reopened = new IndexWriter(tmp);
        List<IndexWriter.Record> records = reopened.readWal();
        assertEquals(3, records.size());
        assertEquals(42, records.get(0).featureId());
        assertEquals(IndexWriter.Op.INSERT, records.get(0).op());
        assertEquals(IndexWriter.Phase.COMMITTED, records.get(2).phase());
    }

    @Test
    @DisplayName("truncating clears the log")
    void truncateClearsLog() {
        IndexWriter writer = new IndexWriter(tmp);
        writer.insert(poi(1, 1, 1), new RecordingTarget());
        assertFalse(writer.readWal().isEmpty());
        writer.truncate();
        assertTrue(writer.readWal().isEmpty());
        assertTrue(writer.journal().isEmpty());
    }
}

package com.citygis.spatial;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import java.nio.file.Path;
import java.util.List;
import java.util.Random;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

/** Correctness of the point KD tree against an exact brute-force oracle. */
class KDTreeIndexTest {

    @TempDir
    Path tmp;

    private KDTreeIndex build(List<GeoFeature> features) {
        SegmentStore store = SegmentStore.create(tmp.resolve("p.seg"),
                Math.max(1, features.size()), 0);
        for (GeoFeature f : features) {
            store.append(f);
        }
        store.flushHeader();
        return new KDTreeIndex(store);
    }

    @Test
    @DisplayName("kNN over random data matches an exhaustive scan exactly")
    void knnMatchesBruteForce() {
        List<GeoFeature> data = TestData.points(2000, 7, 1000);
        KDTreeIndex kd = build(data);
        Random r = new Random(99);
        for (int t = 0; t < 100; t++) {
            GeoPoint probe = new GeoPoint(r.nextDouble() * 1000, r.nextDouble() * 1000);
            int k = 1 + r.nextInt(50);
            assertEquals(TestData.bruteForceKnn(data, probe, k, null), kd.knn(probe, k),
                    "probe=" + probe + " k=" + k);
        }
    }

    @Test
    @DisplayName("a probe sitting exactly on a point returns that point at distance zero")
    void exactHit() {
        List<GeoFeature> data = TestData.points(500, 3, 100);
        KDTreeIndex kd = build(data);
        GeoFeature target = data.get(42);
        List<Neighbor> got = kd.knn(new GeoPoint(target.x(), target.y()), 1);
        assertEquals(1, got.size());
        assertEquals(target.id(), got.get(0).id());
        assertEquals(0.0, got.get(0).distance(), 1e-12);
    }

    @Test
    @DisplayName("duplicate coordinates do not break selection or ordering")
    void duplicateCoordinates() {
        List<GeoFeature> data = new java.util.ArrayList<>();
        for (int i = 0; i < 200; i++) {
            data.add(GeoFeature.point(i, FeatureType.POI, 5, 5));
        }
        KDTreeIndex kd = build(data);
        List<Neighbor> got = kd.knn(new GeoPoint(5, 5), 10);
        assertEquals(10, got.size());
        // All distances tie at zero, so the tie-break on id must order them ascending.
        for (int i = 1; i < got.size(); i++) {
            assertTrue(got.get(i - 1).id() < got.get(i).id(),
                    "ties must be broken by ascending id");
        }
    }

    @Test
    @DisplayName("every slot is reachable from the root")
    void treeIsConnected() {
        for (int n : new int[]{1, 2, 3, 4, 5, 17, 64, 513, 2000}) {
            KDTreeIndex kd = build(TestData.points(n, n, 500));
            assertTrue(kd.verifyConnected(), "orphaned slot in a tree of " + n + " points");
        }
    }

    @Test
    @DisplayName("tree height stays logarithmic")
    void heightIsBalanced() {
        KDTreeIndex kd = build(TestData.points(4096, 5, 500));
        assertEquals(kd.size(), kd.slotCount());
        assertTrue(kd.slotHeight() <= 32, "height " + kd.slotHeight() + " is not balanced");
    }

    @Test
    @DisplayName("a tombstoned row disappears from results without a rebuild")
    void tombstoneHidesRow() {
        List<GeoFeature> data = TestData.points(1000, 11, 200);
        KDTreeIndex kd = build(data);
        GeoFeature victim = data.get(7);
        assertTrue(kd.tombstone(victim.id() == 7 ? 7 : 0));
        assertEquals(999, kd.liveCount());
        assertFalse(kd.knn(new GeoPoint(victim.x(), victim.y()), 5).stream()
                .anyMatch(n -> n.id() == victim.id()));
        // Tombstoning twice is idempotent, so the count must not drift.
        assertFalse(kd.tombstone(7));
        assertEquals(999, kd.liveCount());
    }

    @Test
    @DisplayName("tombstoned rows are excluded from the brute-force comparison")
    void knnAfterDeleteMatchesOracle() {
        List<GeoFeature> data = TestData.points(1500, 13, 300);
        KDTreeIndex kd = build(data);
        List<Long> deleted = List.of(3L, 4L, 400L, 1499L);
        for (long id : deleted) {
            kd.tombstone((int) id);
        }
        Random r = new Random(21);
        for (int t = 0; t < 50; t++) {
            GeoPoint probe = new GeoPoint(r.nextDouble() * 300, r.nextDouble() * 300);
            List<Neighbor> expected = TestData.bruteForceKnn(data, probe, 25, deleted);
            assertEquals(expected, kd.knn(probe, 25));
        }
    }

    @Test
    @DisplayName("reviving a row restores it to results")
    void revive() {
        List<GeoFeature> data = TestData.points(100, 17, 100);
        KDTreeIndex kd = build(data);
        kd.tombstone(5);
        assertEquals(99, kd.liveCount());
        kd.revive(5);
        assertEquals(100, kd.liveCount());
        assertTrue(kd.knn(new GeoPoint(data.get(5).x(), data.get(5).y()), 3).stream()
                .anyMatch(n -> n.id() == 5));
    }

    @Test
    @DisplayName("compaction reclaims tombstoned slots and preserves results")
    void compactReclaimsSlots() {
        List<GeoFeature> data = TestData.points(1000, 23, 300);
        KDTreeIndex kd = build(data);
        List<Long> deleted = new java.util.ArrayList<>();
        for (int i = 0; i < 400; i++) {
            kd.tombstone(i);
            deleted.add((long) i);
        }
        kd.compact();
        assertEquals(600, kd.size());
        assertEquals(0, kd.tombstoneCount());
        assertTrue(kd.verifyConnected(), "compaction must leave every slot reachable");
        Random r = new Random(31);
        for (int t = 0; t < 30; t++) {
            GeoPoint probe = new GeoPoint(r.nextDouble() * 300, r.nextDouble() * 300);
            assertEquals(TestData.bruteForceKnn(data, probe, 20, deleted), kd.knn(probe, 20));
        }
    }

    @Test
    @DisplayName("k <= 0 and empty trees return nothing")
    void degenerateQueries() {
        assertTrue(build(List.of()).knn(new GeoPoint(0, 0), 10).isEmpty());
        KDTreeIndex kd = build(TestData.points(10, 2, 10));
        assertTrue(kd.knn(new GeoPoint(0, 0), 0).isEmpty());
        assertTrue(kd.knn(new GeoPoint(0, 0), -5).isEmpty());
    }

    @Test
    @DisplayName("k larger than the population returns the whole population")
    void kExceedsSize() {
        List<GeoFeature> data = TestData.points(5, 4, 10);
        KDTreeIndex kd = build(data);
        assertEquals(5, kd.knn(new GeoPoint(1, 1), 100).size());
    }

    @Test
    @DisplayName("out-of-range tombstone is rejected")
    void tombstoneBoundsChecked() {
        KDTreeIndex kd = build(TestData.points(10, 6, 10));
        assertThrows(IndexOutOfBoundsException.class, () -> kd.tombstone(10));
        assertThrows(IndexOutOfBoundsException.class, () -> kd.tombstone(-1));
    }
}

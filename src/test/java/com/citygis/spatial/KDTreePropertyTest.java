package com.citygis.spatial;

import net.jqwik.api.Arbitraries;
import net.jqwik.api.ForAll;
import net.jqwik.api.Property;
import net.jqwik.api.constraints.DoubleRange;
import net.jqwik.api.constraints.IntRange;
import net.jqwik.api.constraints.LongRange;

import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.List;
import java.util.Random;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * Property-based verification of the KD tree: for any coordinates, any K and any set of deletions,
 * the index must return exactly the same Top-K as an exhaustive scan.
 */
class KDTreePropertyTest {

    private static final Path TMP = createTempRoot();

    private static Path createTempRoot() {
        try {
            return Files.createTempDirectory("kdprop");
        } catch (java.io.IOException e) {
            throw new ExceptionInInitializerError(e);
        }
    }

    /** Builds a KD tree over {@code n} features generated from a seed. */
    private static KDTreeIndex tree(int n, long seed) {
        Random r = new Random(seed);
        List<GeoFeature> data = new ArrayList<>(n);
        for (int i = 0; i < n; i++) {
            data.add(GeoFeature.point(i, FeatureType.POI, r.nextDouble() * 1000, r.nextDouble() * 1000));
        }
        try {
            Path dir = Files.createTempDirectory(TMP, "t");
            SegmentStore store = SegmentStore.create(dir.resolve("p.seg"), n + 8, 0);
            for (GeoFeature f : data) {
                store.append(f);
            }
            store.flushHeader();
            return new KDTreeIndex(store);
        } catch (java.io.IOException e) {
            throw new IllegalStateException(e);
        }
    }

    private static List<GeoFeature> data(int n, long seed) {
        Random r = new Random(seed);
        List<GeoFeature> data = new ArrayList<>(n);
        for (int i = 0; i < n; i++) {
            data.add(GeoFeature.point(i, FeatureType.POI, r.nextDouble() * 1000, r.nextDouble() * 1000));
        }
        return data;
    }

    @Property(tries = 40)
    void knnEqualsExhaustiveScan(
            @ForAll @IntRange(min = 1, max = 300) int n,
            @ForAll @LongRange(min = 0, max = 1_000_000) long seed,
            @ForAll @IntRange(min = 1, max = 25) int k,
            @ForAll @DoubleRange(min = 0.0, max = 1.0) double px,
            @ForAll @DoubleRange(min = 0.0, max = 1.0) double py) {
        List<GeoFeature> data = data(n, seed);
        KDTreeIndex kd = tree(n, seed);
        GeoPoint probe = new GeoPoint(px * 1000, py * 1000);

        List<Neighbor> expected = new ArrayList<>();
        for (GeoFeature f : data) {
            expected.add(new Neighbor(f, probe.distanceTo(f.x(), f.y())));
        }
        expected.sort(Comparator.naturalOrder());
        List<Neighbor> want = expected.subList(0, Math.min(k, expected.size()));

        assertEquals(want, kd.knn(probe, k),
                "n=" + n + " k=" + k + " probe=(" + probe.x() + "," + probe.y() + ")");
    }

    @Property(tries = 30)
    void knnHonoursTombstones(
            @ForAll @IntRange(min = 2, max = 200) int n,
            @ForAll @LongRange(min = 0, max = 1_000_000) long seed,
            @ForAll @IntRange(min = 1, max = 20) int k) {
        List<GeoFeature> data = data(n, seed);
        KDTreeIndex kd = tree(n, seed);
        Random r = new Random(seed ^ 0x5DEECE66DL);
        List<Long> deleted = new ArrayList<>();
        int toDelete = r.nextInt(n);
        for (int i = 0; i < toDelete; i++) {
            int row = r.nextInt(n);
            if (kd.tombstone(row)) {
                deleted.add((long) row);
            }
        }
        GeoPoint probe = new GeoPoint(r.nextDouble() * 1000, r.nextDouble() * 1000);
        List<Neighbor> expected = new ArrayList<>();
        for (GeoFeature f : data) {
            if (!deleted.contains(f.id())) {
                expected.add(new Neighbor(f, probe.distanceTo(f.x(), f.y())));
            }
        }
        expected.sort(Comparator.naturalOrder());
        List<Neighbor> want = expected.subList(0, Math.min(k, expected.size()));
        assertEquals(want, kd.knn(probe, k));
    }

    @Property(tries = 25)
    void knnResultIsSortedAndDistinct(
            @ForAll @IntRange(min = 2, max = 150) int n,
            @ForAll @LongRange(min = 0, max = 1_000_000) long seed,
            @ForAll @IntRange(min = 1, max = 30) int k) {
        KDTreeIndex kd = tree(n, seed);
        Random r = new Random(seed);
        List<Neighbor> got = kd.knn(new GeoPoint(r.nextDouble() * 1000, r.nextDouble() * 1000), k);
        for (int i = 1; i < got.size(); i++) {
            assertTrue(got.get(i - 1).compareTo(got.get(i)) < 0,
                    "results must be strictly ordered and duplicate-free");
        }
    }

    @Property(tries = 20)
    void everySlotStaysReachable(@ForAll @IntRange(min = 1, max = 200) int n,
                                  @ForAll @LongRange(min = 0, max = 1_000_000) long seed) {
        assertTrue(tree(n, seed).verifyConnected(), "the tree must not orphan any slot");
    }

    @Property(tries = 15)
    void compactionPreservesResults(
            @ForAll @IntRange(min = 2, max = 120) int n,
            @ForAll @LongRange(min = 0, max = 1_000_000) long seed) {
        List<GeoFeature> data = data(n, seed);
        KDTreeIndex kd = tree(n, seed);
        Random r = new Random(seed + 1);
        List<Long> deleted = new ArrayList<>();
        for (int i = 0; i < n; i++) {
            if (r.nextBoolean() && kd.tombstone(i)) {
                deleted.add((long) i);
            }
        }
        kd.compact();
        GeoPoint probe = new GeoPoint(r.nextDouble() * 1000, r.nextDouble() * 1000);
        List<Neighbor> expected = new ArrayList<>();
        for (GeoFeature f : data) {
            if (!deleted.contains(f.id())) {
                expected.add(new Neighbor(f, probe.distanceTo(f.x(), f.y())));
            }
        }
        expected.sort(Comparator.naturalOrder());
        assertEquals(expected.subList(0, Math.min(10, expected.size())), kd.knn(probe, 10));
    }
}

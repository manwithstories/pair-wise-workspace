package com.citygis.spatial;

import net.jqwik.api.ForAll;
import net.jqwik.api.Property;
import net.jqwik.api.constraints.DoubleRange;
import net.jqwik.api.constraints.IntRange;
import net.jqwik.api.constraints.LongRange;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.Random;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * Property-based verification of the parallel query path: whatever the segment layout, the
 * parallel answer must be indistinguishable from a single-threaded scan.
 */
class ParallelQueryPropertyTest {

    private static final Path TMP = createTempRoot();

    private static Path createTempRoot() {
        try {
            return Files.createTempDirectory("parprop");
        } catch (IOException e) {
            throw new ExceptionInInitializerError(e);
        }
    }

    /** Builds a multi-segment index and hands back the flattened corpus. */
    private record Fixture(SpatialIndex index, List<GeoFeature> points) {
    }

    private Fixture build(int segments, int perSegment, long seed, long budget) {
        try {
            Path root = Files.createTempDirectory(TMP, "idx");
            Random r = new Random(seed);
            List<GeoFeature> data = new ArrayList<>();
            for (int i = 0; i < segments * perSegment; i++) {
                data.add(GeoFeature.point(i, FeatureType.POI, r.nextDouble() * 1000,
                        r.nextDouble() * 1000));
            }
            return new Fixture(SpatialIndex.build(root, data, budget), data);
        } catch (IOException e) {
            throw new IllegalStateException(e);
        }
    }

    @Property(tries = 15)
    void parallelKnnEqualsSequential(
            @ForAll @IntRange(min = 1, max = 6) int segments,
            @ForAll @IntRange(min = 1, max = 60) int perSegment,
            @ForAll @LongRange(min = 0, max = 1_000_000) long seed,
            @ForAll @IntRange(min = 1, max = 20) int k,
            @ForAll @DoubleRange(min = 0.0, max = 1.0) double px,
            @ForAll @DoubleRange(min = 0.0, max = 1.0) double py) {
        Fixture f = build(segments, perSegment, seed, 512L * 1024);
        try (SpatialIndex index = f.index()) {
            GeoPoint probe = new GeoPoint(px * 1000, py * 1000);
            assertEquals(TestData.bruteForceKnn(f.points(), probe, k, null),
                    index.knn(probe, k),
                    "segments=" + segments + " k=" + k + " probe=" + probe);
        }
    }

    @Property(tries = 12)
    void parallelKnnIsStableAcrossRepeatedRuns(
            @ForAll @IntRange(min = 1, max = 5) int segments,
            @ForAll @IntRange(min = 10, max = 80) int perSegment,
            @ForAll @LongRange(min = 0, max = 1_000_000) long seed) {
        Fixture f = build(segments, perSegment, seed, 512L * 1024);
        try (SpatialIndex index = f.index()) {
            GeoPoint probe = new GeoPoint(123.456, 789.012);
            List<Neighbor> first = index.knn(probe, 15);
            for (int i = 0; i < 5; i++) {
                assertEquals(first, index.knn(probe, 15),
                        "repeated identical queries must return identical results");
            }
        }
    }

    @Property(tries = 10)
    void rangeSearchMatchesBruteForceUnderSegmentation(
            @ForAll @IntRange(min = 1, max = 4) int segments,
            @ForAll @IntRange(min = 5, max = 60) int perSegment,
            @ForAll @LongRange(min = 0, max = 1_000_000) long seed,
            @ForAll @DoubleRange(min = 0.0, max = 1.0) double wx,
            @ForAll @DoubleRange(min = 0.0, max = 1.0) double wy,
            @ForAll @DoubleRange(min = 0.0, max = 1.0) double size) {
        try {
            Path root = Files.createTempDirectory(TMP, "r");
            Random r = new Random(seed);
            List<GeoFeature> data = TestData.parcels(segments * perSegment, seed, 1000);
            try (SpatialIndex index = SpatialIndex.build(root, data, 512L * 1024)) {
                // Guarantee a non-degenerate window: BBox rejects an inverted range outright.
                double x0 = wx * 800;
                double y0 = wy * 800;
                double side = 1 + size * 200;
                BBox window = new BBox(x0, y0, x0 + side, y0 + side);
                assertEquals(TestData.bruteForceRange(data, window),
                        TestData.ids(index.rangeSearch(window)));
            }
        } catch (IOException e) {
            throw new IllegalStateException(e);
        }
    }

    @Property(tries = 10)
    void mergeIsIndependentOfChunking(
            @ForAll @LongRange(min = 0, max = 1_000_000) long seed,
            @ForAll @IntRange(min = 1, max = 20) int k) {
        Random r = new Random(seed);
        List<Neighbor> pool = new ArrayList<>();
        for (int i = 0; i < 300; i++) {
            pool.add(new Neighbor(GeoFeature.point(i, FeatureType.POI, r.nextDouble(),
                    r.nextDouble()), r.nextDouble()));
        }
        // Take the expected answer from a sorted copy, then shuffle the working list freely: the
        // merge must not depend on the order the chunks arrive in.
        List<Neighbor> sorted = new ArrayList<>(pool);
        sorted.sort(Neighbor::compareTo);
        List<Neighbor> want = sorted.subList(0, Math.min(k, sorted.size()));
        for (int trial = 0; trial < 8; trial++) {
            List<Neighbor> shuffled = new ArrayList<>(pool);
            java.util.Collections.shuffle(shuffled, r);
            List<List<Neighbor>> chunks = new ArrayList<>();
            int i = 0;
            while (i < shuffled.size()) {
                int sz = 1 + r.nextInt(12);
                chunks.add(new ArrayList<>(shuffled.subList(i, Math.min(shuffled.size(), i + sz))));
                i += sz;
            }
            assertEquals(want, ParallelQueryExecutor.mergeTopK(chunks, k));
        }
    }

    @Property(tries = 8)
    void deletingAFeatureNeverResurrectsIt(
            @ForAll @IntRange(min = 20, max = 200) int n,
            @ForAll @LongRange(min = 0, max = 1_000_000) long seed) {
        try {
            Path root = Files.createTempDirectory(TMP, "del");
            Random r = new Random(seed);
            List<GeoFeature> data = new ArrayList<>();
            for (int i = 0; i < n; i++) {
                data.add(GeoFeature.point(i, FeatureType.POI, r.nextDouble() * 1000,
                        r.nextDouble() * 1000));
            }
            try (SpatialIndex index = SpatialIndex.build(root, data, 512L * 1024)) {
                List<Long> deleted = new ArrayList<>();
                for (int i = 0; i < 5; i++) {
                    long victim = data.get(r.nextInt(n)).id();
                    index.delete(victim);
                    deleted.add(victim);
                }
                List<Neighbor> all = index.knn(new GeoPoint(500, 500), 100_000);
                for (long gone : deleted) {
                    assertTrue(all.stream().noneMatch(x -> x.id() == gone),
                            "deleted feature " + gone + " must never reappear");
                }
            }
        } catch (IOException e) {
            throw new IllegalStateException(e);
        }
    }
}

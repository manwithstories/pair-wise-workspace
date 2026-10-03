package com.citygis.spatial;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import java.nio.file.Path;
import java.util.ArrayList;
import java.util.HashSet;
import java.util.List;
import java.util.Random;
import java.util.Set;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

/** End-to-end behaviour of the unified façade across build, query and incremental write. */
class SpatialIndexTest {

    @TempDir
    Path tmp;

    private static final long BUDGET = 32L * 1024 * 1024;

    private List<GeoFeature> dataset(int points, int parcels, long seed) {
        return TestData.merge(TestData.points(points, seed, 1000),
                TestData.parcels(parcels, seed + 1, 1000));
    }

    @Test
    @DisplayName("a built index answers kNN exactly as an exhaustive scan would")
    void knnMatchesBruteForce() {
        List<GeoFeature> data = dataset(3000, 1000, 1);
        try (SpatialIndex index = SpatialIndex.build(tmp.resolve("idx"), data, BUDGET)) {
            assertEquals(4000, index.featureCount());
            Random r = new Random(5);
            for (int t = 0; t < 60; t++) {
                GeoPoint probe = new GeoPoint(r.nextDouble() * 1000, r.nextDouble() * 1000);
                assertEquals(TestData.bruteForceKnn(TestData.points(3000, 1, 1000), probe, 25, null),
                        index.knn(probe, 25));
            }
        }
    }

    @Test
    @DisplayName("parallel and sequential kNN always agree")
    void parallelEqualsSequential() {
        try (SpatialIndex index = SpatialIndex.build(tmp.resolve("idx"), dataset(5000, 500, 2),
                BUDGET)) {
            Random r = new Random(9);
            for (int t = 0; t < 100; t++) {
                GeoPoint probe = new GeoPoint(r.nextDouble() * 1000, r.nextDouble() * 1000);
                assertEquals(index.knnSequential(probe, 50), index.knn(probe, 50));
            }
        }
    }

    @Test
    @DisplayName("range selection matches a brute-force envelope scan")
    void rangeMatchesBruteForce() {
        List<GeoFeature> parcels = TestData.parcels(2000, 11, 1000);
        try (SpatialIndex index = SpatialIndex.build(tmp.resolve("idx"), parcels, BUDGET)) {
            Random r = new Random(13);
            for (int t = 0; t < 40; t++) {
                double x = r.nextDouble() * 900;
                double y = r.nextDouble() * 900;
                BBox w = new BBox(x, y, x + 100, y + 100);
                assertEquals(TestData.bruteForceRange(parcels, w), TestData.ids(index.rangeSearch(w)));
            }
        }
    }

    @Test
    @DisplayName("point-in-parcel lookup matches a brute-force ray cast")
    void containmentMatchesBruteForce() {
        List<GeoFeature> parcels = TestData.parcels(1500, 17, 1000);
        try (SpatialIndex index = SpatialIndex.build(tmp.resolve("idx"), parcels, BUDGET)) {
            Random r = new Random(19);
            for (int t = 0; t < 40; t++) {
                double x = r.nextDouble() * 1000;
                double y = r.nextDouble() * 1000;
                assertEquals(TestData.bruteForceContaining(parcels, x, y),
                        TestData.ids(index.containingParcels(x, y)));
            }
        }
    }

    @Test
    @DisplayName("an inserted feature is immediately visible to both indexes")
    void insertIsVisibleEverywhere() {
        try (SpatialIndex index = SpatialIndex.build(tmp.resolve("idx"),
                dataset(1000, 200, 21), BUDGET)) {
            GeoFeature np = GeoFeature.point(9_000_001, FeatureType.POI, 500, 500);
            index.insert(np);
            assertEquals(np.id(), index.knn(new GeoPoint(500, 500), 1).get(0).id(),
                    "the new point must be the nearest to its own coordinates");
            assertEquals(1001, index.generation().kdTrees().get(0).store().count());

            GeoFeature parcel = GeoFeature.polygon(9_000_002, FeatureType.PARCEL,
                    new double[]{495, 495, 505, 495, 505, 505, 495, 505});
            index.insert(parcel);
            assertTrue(index.containingParcels(500, 500).stream()
                    .anyMatch(f -> f.id() == parcel.id()), "the new parcel must contain its centre");
        }
    }

    @Test
    @DisplayName("a deleted feature disappears from both indexes")
    void deleteRemovesFromBothIndexes() {
        List<GeoFeature> points = TestData.points(1000, 23, 1000);
        try (SpatialIndex index = SpatialIndex.build(tmp.resolve("idx"), points, BUDGET)) {
            GeoFeature victim = points.get(11);
            assertTrue(index.delete(victim.id()));
            assertFalse(index.knn(new GeoPoint(victim.x(), victim.y()), 20).stream()
                    .anyMatch(n -> n.id() == victim.id()), "deleted point still returned");
            // The row physically remains in the segment; only its tombstone flag is set, which is
            // what makes the delete lazy (no rebuild) while still hiding it from queries.
            assertEquals(1000, index.generation().kdTrees().get(0).store().count(),
                    "a lazy delete must not reclaim the row");
            assertEquals(1, index.generation().kdTrees().get(0).tombstoneCount(),
                    "exactly one row must be retired");
            assertEquals(999, index.generation().kdTrees().get(0).liveCount());
            assertFalse(index.delete(victim.id()), "deleting twice must report no change");
        }
    }

    @Test
    @DisplayName("a deleted parcel stops being returned by range and containment queries")
    void deleteParcelIsConsistent() {
        List<GeoFeature> parcels = TestData.parcels(500, 29, 1000);
        try (SpatialIndex index = SpatialIndex.build(tmp.resolve("idx"), parcels, BUDGET)) {
            GeoFeature victim = parcels.get(7);
            double cx = (BBox.envelope(victim.vertices()).minX()
                    + BBox.envelope(victim.vertices()).maxX()) / 2;
            double cy = (BBox.envelope(victim.vertices()).minY()
                    + BBox.envelope(victim.vertices()).maxY()) / 2;
            assertTrue(index.containingParcels(cx, cy).stream().anyMatch(f -> f.id() == victim.id()));
            index.delete(victim.id());
            assertFalse(index.containingParcels(cx, cy).stream()
                    .anyMatch(f -> f.id() == victim.id()), "deleted parcel still contains its centre");
            BBox whole = new BBox(0, 0, 1000, 1000);
            assertFalse(TestData.ids(index.rangeSearch(whole)).contains(victim.id()));
        }
    }

    @Test
    @DisplayName("both indexes stay consistent across a mixed insert/delete workload")
    void mixedWorkloadKeepsTreesConsistent() {
        try (SpatialIndex index = SpatialIndex.build(tmp.resolve("idx"),
                dataset(1500, 500, 31), BUDGET)) {
            Random r = new Random(37);
            List<Long> inserted = new ArrayList<>();
            List<Long> deleted = new ArrayList<>();
            long nextId = 1_000_000;
            for (int step = 0; step < 40; step++) {
                if (inserted.isEmpty() || r.nextBoolean()) {
                    long id = nextId++;
                    index.insert(GeoFeature.point(id, FeatureType.POI,
                            r.nextDouble() * 1000, r.nextDouble() * 1000));
                    inserted.add(id);
                } else {
                    long id = inserted.remove(r.nextInt(inserted.size()));
                    index.delete(id);
                    deleted.add(id);
                }
            }
            // Every inserted id must be findable and no deleted id may appear.
            // Ask for the whole corpus; the index clamps a request larger than the population.
            List<Neighbor> all = index.knn(new GeoPoint(500, 500), 100_000);
            Set<Long> found = new HashSet<>();
            for (Neighbor n : all) {
                found.add(n.id());
            }
            assertTrue(found.containsAll(inserted), "an inserted feature went missing");
            for (long gone : deleted) {
                assertFalse(found.contains(gone), "a deleted feature came back");
            }
        }
    }

    @Test
    @DisplayName("a failed transaction leaves the index serving its previous contents")
    void failedWriteChangesNothing() {
        try (SpatialIndex index = SpatialIndex.build(tmp.resolve("idx"),
                TestData.points(500, 41, 1000), BUDGET)) {
            List<Neighbor> before = index.knn(new GeoPoint(500, 500), 30);
            index.writer().setFailureInjector((txn, phase) -> phase == IndexWriter.Phase.COMMITTED);
            assertThrows(IndexWriter.TransactionFailedException.class,
                    () -> index.insert(GeoFeature.point(7_777_777, FeatureType.POI, 500, 500)));
            index.writer().setFailureInjector(null);
            assertEquals(before, index.knn(new GeoPoint(500, 500), 30),
                    "a rolled-back write must not disturb the index");
        }
    }

    @Test
    @DisplayName("the index reopens from disk with identical query answers")
    void reopenFromDisk() {
        List<GeoFeature> data = dataset(2000, 800, 43);
        Path root = tmp.resolve("idx");
        List<Neighbor> before;
        try (SpatialIndex index = SpatialIndex.build(root, data, BUDGET)) {
            before = index.knn(new GeoPoint(321, 654), 50);
        }
        try (SpatialIndex reopened = SpatialIndex.open(root, BUDGET)) {
            assertEquals(2800, reopened.featureCount());
            assertEquals(before, reopened.knn(new GeoPoint(321, 654), 50));
        }
    }

    @Test
    @DisplayName("many segments produce the same answers as a single segment")
    void segmentationDoesNotChangeResults() {
        List<GeoFeature> data = TestData.points(4000, 47, 1000);
        List<Neighbor> single;
        List<Neighbor> many;
        Random r = new Random(53);
        GeoPoint probe = new GeoPoint(r.nextDouble() * 1000, r.nextDouble() * 1000);
        try (SpatialIndex index = SpatialIndex.build(tmp.resolve("one"), data, 256L * 1024 * 1024)) {
            assertEquals(1, index.segmentCount());
            single = index.knn(probe, 40);
        }
        try (SpatialIndex index = SpatialIndex.build(tmp.resolve("many"), data, 512L * 1024)) {
            assertTrue(index.segmentCount() > 1, "a small budget must split into several segments");
            many = index.knn(probe, 40);
        }
        assertEquals(single, many, "segmentation must not change the answer");
        assertEquals(TestData.bruteForceKnn(data, probe, 40, null), many);
    }

    @Test
    @DisplayName("rebuild republishes a consistent generation")
    void rebuildKeepsAnswers() {
        try (SpatialIndex index = SpatialIndex.build(tmp.resolve("idx"),
                dataset(2000, 500, 59), BUDGET)) {
            index.insert(GeoFeature.point(8_888_888, FeatureType.POI, 250, 750));
            index.delete(3);
            List<Neighbor> before = index.knn(new GeoPoint(250, 750), 20);
            index.rebuild();
            assertEquals(before, index.knn(new GeoPoint(250, 750), 20),
                    "a rebuild must preserve query results");
        }
    }

    @Test
    @DisplayName("merging segments preserves the answer")
    void mergeSegmentsPreservesAnswers() {
        try (SpatialIndex index = SpatialIndex.build(tmp.resolve("idx"),
                TestData.points(4000, 0, 61), 512L * 1024)) {
            assertTrue(index.segmentCount() > 1);
            List<Neighbor> before = index.knn(new GeoPoint(400, 400), 30);
            index.compactSegments();
            assertEquals(1, index.segmentCount());
            assertEquals(before, index.knn(new GeoPoint(400, 400), 30));
        }
    }

    @Test
    @DisplayName("an empty index answers queries without failing")
    void emptyIndex() {
        try (SpatialIndex index = SpatialIndex.build(tmp.resolve("idx"), List.<GeoFeature>of(),
                BUDGET)) {
            assertTrue(index.knn(new GeoPoint(0, 0), 10).isEmpty());
            assertTrue(index.rangeSearch(new BBox(0, 0, 1, 1)).isEmpty());
            assertTrue(index.containingParcels(0, 0).isEmpty());
        }
    }

    @Test
    @DisplayName("polygon range windows refine to exact geometry")
    void polygonSearchUsesExactGeometry() {
        List<GeoFeature> parcels = new ArrayList<>();
        for (int i = 0; i < 30; i++) {
            parcels.add(GeoFeature.polygon(i, FeatureType.PARCEL,
                    new double[]{i * 10.0, 0, i * 10.0 + 8, 0, i * 10.0 + 8, 10, i * 10.0, 10}));
        }
        try (SpatialIndex index = SpatialIndex.build(tmp.resolve("idx"), parcels, BUDGET)) {
            double[] window = {5, -5, 25, 15};
            List<GeoFeature> got = index.searchWithinPolygon(window);
            assertFalse(got.isEmpty());
            for (GeoFeature f : got) {
                assertTrue(RTreeIndex.polygonsOverlap(f.vertices(), window),
                        "parcel " + f.id() + " does not truly overlap the window");
            }
        }
    }
}

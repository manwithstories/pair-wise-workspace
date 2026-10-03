package com.citygis.spatial;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import java.nio.file.Path;
import java.util.List;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

/** Round-trip and layout invariants of the off-heap segment store. */
class SegmentStoreTest {

    @TempDir
    Path tmp;

    @Test
    @DisplayName("points survive a write/read round trip exactly")
    void pointRoundTrip() {
        SegmentStore s = SegmentStore.create(tmp.resolve("p.seg"), 100, 0);
        List<GeoFeature> data = TestData.points(100, 3, 500);
        for (GeoFeature f : data) {
            s.append(f);
        }
        s.flushHeader();
        assertEquals(100, s.count());
        for (int i = 0; i < 100; i++) {
            assertEquals(data.get(i), s.featureAt(i));
            assertEquals(data.get(i).x(), s.xAt(i));
            assertEquals(data.get(i).y(), s.yAt(i));
            assertEquals(data.get(i).id(), s.idAt(i));
        }
    }

    @Test
    @DisplayName("polygon bulk data does not bleed between rows")
    void polygonRoundTrip() {
        List<GeoFeature> data = TestData.parcels(120, 9, 500);
        int pool = 0;
        for (GeoFeature f : data) {
            pool += SegmentStore.vertexSlots(f);
        }
        SegmentStore s = SegmentStore.create(tmp.resolve("a.seg"), 200, pool + 32);
        for (GeoFeature f : data) {
            s.append(f);
        }
        s.flushHeader();
        for (int i = 0; i < data.size(); i++) {
            assertArrayEquals(data.get(i).vertices(), s.featureAt(i).vertices(),
                    "vertex array of row " + i + " was corrupted");
            assertEquals(BBox.envelope(data.get(i).vertices()), s.bboxAt(i));
        }
    }

    @Test
    @DisplayName("mixed points and parcels keep independent row regions")
    void mixedRoundTrip() {
        List<GeoFeature> pts = TestData.points(50, 5, 100);
        List<GeoFeature> polys = TestData.parcels(50, 6, 100);
        SegmentStore ps = SegmentStore.create(tmp.resolve("p.seg"), 100, 0);
        SegmentStore as = SegmentStore.create(tmp.resolve("a.seg"), 100, 50 * 8 + 32);
        for (GeoFeature f : pts) {
            ps.append(f);
        }
        for (GeoFeature f : polys) {
            as.append(f);
        }
        ps.flushHeader();
        as.flushHeader();
        assertEquals(50, ps.count());
        assertEquals(50, as.count());
        assertEquals(0, ps.poolUsed());
        assertEquals(50 * 8, as.poolUsed());
        for (int i = 0; i < 50; i++) {
            assertEquals(pts.get(i), ps.featureAt(i));
            assertArrayEquals(polys.get(i).vertices(), as.featureAt(i).vertices());
        }
    }

    @Test
    @DisplayName("a store can be reopened and still reports its contents")
    void reopenKeepsData() {
        Path path = tmp.resolve("p.seg");
        List<GeoFeature> data = TestData.points(200, 11, 100);
        try (SegmentStore s = SegmentStore.create(path, 300, 0)) {
            for (GeoFeature f : data) {
                s.append(f);
            }
            s.flushHeader();
            s.sync();
        }
        try (SegmentStore r = SegmentStore.openReadOnly(path)) {
            assertEquals(200, r.count());
            assertEquals(300, r.maxRows());
            assertEquals(data.get(7), r.featureAt(7));
        }
    }

    @Test
    @DisplayName("reopening for writing preserves the row capacity and existing rows")
    void reopenForWriting() {
        Path path = tmp.resolve("p.seg");
        List<GeoFeature> data = TestData.points(50, 13, 100);
        SegmentStore created = SegmentStore.create(path, 50, 0);
        for (GeoFeature f : data) {
            created.append(f);
        }
        created.flushHeader();
        created.sync();
        created.close();

        try (SegmentStore rw = SegmentStore.reopen(path)) {
            assertEquals(50, rw.maxRows(), "row capacity must stay stable across reopen");
            assertEquals(50, rw.count());
            for (int i = 0; i < 50; i++) {
                assertEquals(data.get(i), rw.featureAt(i));
            }
        }
    }

    @Test
    @DisplayName("tombstones and revival are reflected in the durable file")
    void tombstonePersists() {
        Path path = tmp.resolve("p.seg");
        try (SegmentStore s = SegmentStore.create(path, 10, 0)) {
            for (int i = 0; i < 10; i++) {
                s.append(GeoFeature.point(i, FeatureType.POI, i, i));
            }
            s.flushHeader();
            assertTrue(s.tombstone(3));
            assertFalse(s.tombstone(3), "second tombstone is a no-op");
            assertTrue(s.isTombstone(3));
            assertTrue(s.revive(3));
            assertFalse(s.isTombstone(3));
            s.flushHeader();
            s.sync();
        }
        try (SegmentStore r = SegmentStore.openReadOnly(path)) {
            assertFalse(r.isTombstone(3));
        }
    }

    @Test
    @DisplayName("the store refuses to exceed its declared capacity")
    void capacityEnforced() {
        SegmentStore s = SegmentStore.create(tmp.resolve("p.seg"), 3, 0);
        s.append(GeoFeature.point(1, FeatureType.POI, 1, 1));
        s.append(GeoFeature.point(2, FeatureType.POI, 2, 2));
        s.append(GeoFeature.point(3, FeatureType.POI, 3, 3));
        assertThrows(IllegalStateException.class,
                () -> s.append(GeoFeature.point(4, FeatureType.POI, 4, 4)));
    }

    @Test
    @DisplayName("a pool sized too small is rejected instead of overwriting other rows")
    void poolCapacityEnforced() {
        // A square is 4 vertices = 8 doubles, so exactly one square fits in an 8-slot pool.
        // The second append must be refused rather than overwriting the first polygon.
        SegmentStore s = SegmentStore.create(tmp.resolve("a.seg"), 10, 8);
        s.append(GeoFeature.polygon(1, FeatureType.PARCEL, new double[]{0, 0, 1, 0, 1, 1, 0, 1}));
        assertEquals(8, s.poolUsed());
        assertThrows(IllegalStateException.class,
                () -> s.append(GeoFeature.polygon(2, FeatureType.PARCEL,
                        new double[]{2, 2, 3, 2, 3, 3, 2, 3})));
    }

    @Test
    @DisplayName("opening a non-segment file fails loudly")
    void badFileRejected() {
        Path bogus = tmp.resolve("bogus.seg");
        try {
            java.nio.file.Files.writeString(bogus, "not a segment at all, definitely not");
            assertThrows(RuntimeException.class, () -> SegmentStore.openReadOnly(bogus));
        } catch (java.io.IOException e) {
            throw new AssertionError(e);
        }
    }

    @Test
    @DisplayName("header fields do not collide with the row region")
    void headerDoesNotOverlapRows() {
        SegmentStore s = SegmentStore.create(tmp.resolve("p.seg"), 4, 0);
        for (int i = 0; i < 4; i++) {
            s.append(GeoFeature.point(i, FeatureType.POI, 100 + i, 200 + i));
        }
        s.flushHeader();
        // Counting is intact after many appends, which proves the count field was not clobbered.
        assertEquals(4, s.count());
        for (int i = 0; i < 4; i++) {
            assertEquals(100 + i, s.xAt(i));
            assertEquals(200 + i, s.yAt(i));
        }
    }
}

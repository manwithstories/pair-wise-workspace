package com.citygis.spatial;

import java.io.IOException;
import java.io.UncheckedIOException;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Collections;
import java.util.Iterator;
import java.util.List;
import java.util.concurrent.locks.ReentrantReadWriteLock;

/**
 * Unified façade over the segmented KD/R index pair.
 *
 * <p>Owns the segment metadata, the built per-segment trees and the write path, and switches
 * between "serving the current generation" and "rebuilding into a new generation" without the
 * caller seeing an inconsistent index: a rebuild publishes a new metadata list atomically under a
 * write lock, while queries only ever observe a complete generation.
 *
 * <p>Typical use:
 * <pre>{@code
 * try (SpatialIndex index = SpatialIndex.build(root, features, heapBudget)) {
 *     List<Neighbor> nearest = index.knn(new GeoPoint(x, y), 50);
 *     List<GeoFeature> parcels = index.rangeSearch(window);
 *     index.insert(GeoFeature.point(id, FeatureType.POI, x, y));
 * }
 * }</pre>
 */
public final class SpatialIndex implements AutoCloseable {

    private final Path root;
    private final SegmentLoader loader;
    private final ParallelQueryExecutor executor;
    private final IndexWriter writer;
    private final long heapBudgetBytes;

    private final ReentrantReadWriteLock lock = new ReentrantReadWriteLock();

    /** Guards the generation fields; replaced wholesale on rebuild. */
    private volatile Generation generation;

    /** An immutable snapshot of everything a query needs. */
    public record Generation(List<SegmentLoader.SegmentMeta> segments,
                             List<KDTreeIndex> kdTrees,
                             List<RTreeIndex> rTrees,
                             long featureCount) {

        public Generation {
            segments = List.copyOf(segments);
            kdTrees = List.copyOf(kdTrees);
            rTrees = List.copyOf(rTrees);
        }

        public int segmentCount() {
            return segments.size();
        }
    }

    private SpatialIndex(Path root, SegmentLoader loader, ParallelQueryExecutor executor,
                         IndexWriter writer, long heapBudgetBytes, Generation generation) {
        this.root = root;
        this.loader = loader;
        this.executor = executor;
        this.writer = writer;
        this.heapBudgetBytes = heapBudgetBytes;
        this.generation = generation;
    }

    /**
     * Builds an index from a feature stream, honouring the heap budget.
     *
     * <p>The stream is consumed once, segment by segment; no more than one segment's worth of data
     * is ever materialised.
     */
    public static SpatialIndex build(Path root, Iterator<GeoFeature> features, long heapBudgetBytes) {
        SegmentLoader loader = new SegmentLoader(root, heapBudgetBytes);
        List<SegmentLoader.SegmentMeta> metas = loader.load(features);
        return open(root, metas, heapBudgetBytes);
    }

    /** Builds from an {@link Iterable} source. */
    public static SpatialIndex build(Path root, Iterable<GeoFeature> features, long heapBudgetBytes) {
        return build(root, features.iterator(), heapBudgetBytes);
    }

    /** Opens an already-built segment tree. */
    public static SpatialIndex open(Path root, long heapBudgetBytes) {
        List<SegmentLoader.SegmentMeta> metas = SegmentLoader.readMetas(root);
        return open(root, metas, heapBudgetBytes);
    }

    private static SpatialIndex open(Path root, List<SegmentLoader.SegmentMeta> metas,
                                     long heapBudgetBytes) {
        SegmentLoader loader = new SegmentLoader(root, heapBudgetBytes);
        List<KDTreeIndex> kd = new ArrayList<>(metas.size());
        List<RTreeIndex> rt = new ArrayList<>(metas.size());
        long total = 0;
        for (SegmentLoader.SegmentMeta meta : metas) {
            if (meta.pointCount() > 0) {
                kd.add(new KDTreeIndex(SegmentStore.openReadOnly(meta.directory()
                        .resolve(SegmentLoader.POINT_FILE))));
            } else {
                kd.add(new KDTreeIndex(emptyStore(meta.directory().resolve(SegmentLoader.POINT_FILE))));
            }
            if (meta.areaCount() > 0) {
                rt.add(new RTreeIndex(SegmentStore.openReadOnly(meta.directory()
                        .resolve(SegmentLoader.AREA_FILE))));
            } else {
                rt.add(new RTreeIndex(emptyStore(meta.directory().resolve(SegmentLoader.AREA_FILE))));
            }
            total += meta.totalCount();
        }
        Generation gen = new Generation(metas, kd, rt, total);
        IndexWriter writer = new IndexWriter(root);
        return new SpatialIndex(root, loader, new ParallelQueryExecutor(), writer, heapBudgetBytes, gen);
    }

    private static SegmentStore emptyStore(Path path) {
        if (java.nio.file.Files.exists(path)) {
            return SegmentStore.openReadOnly(path);
        }
        return SegmentStore.create(path, 1, 1);
    }

    /** Heap budget this index was configured with. */
    public long heapBudgetBytes() {
        return heapBudgetBytes;
    }

    /** The currently served generation. */
    public Generation generation() {
        return generation;
    }

    /** Total live features across all segments. */
    public long featureCount() {
        return generation.featureCount();
    }

    public int segmentCount() {
        return generation.segmentCount();
    }

    public SegmentLoader loader() {
        return loader;
    }

    public ParallelQueryExecutor executor() {
        return executor;
    }

    public IndexWriter writer() {
        return writer;
    }

    // ------------------------------------------------------------------ query

    /**
     * k-nearest-neighbour lookup across every segment.
     *
     * @return up to K neighbours, identical to a single-threaded scan of all segments
     */
    public List<Neighbor> knn(GeoPoint probe, int k) {
        lock.readLock().lock();
        try {
            Generation g = generation;
            return executor.knn(g.segments(), g.kdTrees(), probe, k);
        } finally {
            lock.readLock().unlock();
        }
    }

    /** Sequential kNN over the same segments; the oracle for {@link #knn}. */
    public List<Neighbor> knnSequential(GeoPoint probe, int k) {
        lock.readLock().lock();
        try {
            return ParallelQueryExecutor.knnSequential(generation.kdTrees(), probe, k);
        } finally {
            lock.readLock().unlock();
        }
    }

    /**
     * Rectangle range selection over area features.
     *
     * <p>Wide windows degrade automatically to a parallel linear scan; see
     * {@link ParallelQueryExecutor#rangeSearch}.
     */
    public List<GeoFeature> rangeSearch(BBox window) {
        lock.readLock().lock();
        try {
            Generation g = generation;
            return executor.rangeSearch(g.segments(), g.rTrees(), window);
        } finally {
            lock.readLock().unlock();
        }
    }

    /** Range selection restricted to polygons whose geometry really overlaps the window outline. */
    public List<GeoFeature> searchWithinPolygon(double[] window) {
        lock.readLock().lock();
        try {
            Generation g = generation;
            List<GeoFeature> out = new ArrayList<>();
            BBox envelope = BBox.envelope(window);
            for (int i = 0; i < g.rTrees().size(); i++) {
                if (!g.segments().get(i).bounds().intersects(envelope)) {
                    continue;
                }
                out.addAll(g.rTrees().get(i).searchWithinPolygon(window));
            }
            return out;
        } finally {
            lock.readLock().unlock();
        }
    }

    /** Point-in-polygon lookup across all segments. */
    public List<GeoFeature> containingParcels(double x, double y) {
        lock.readLock().lock();
        try {
            Generation g = generation;
            return executor.containingPoint(g.segments(), g.rTrees(), x, y);
        } finally {
            lock.readLock().unlock();
        }
    }

    // ----------------------------------------------------------------- writes

    /**
     * Inserts a feature into both trees as one transaction.
     *
     * <p>Points are appended to the last segment; the trees are updated in place and the segment
     * metadata is refreshed so subsequent queries route to the new data.
     */
    public void insert(GeoFeature feature) {
        lock.writeLock().lock();
        try {
            Generation g = generation;
            if (g.segments().isEmpty()) {
                throw new IllegalStateException("index has no segments to write into");
            }
            int target = g.segments().size() - 1;
            SegmentLoader.SegmentMeta meta = g.segments().get(target);
            try (SegmentTarget segTarget = SegmentTarget.open(meta, g, target)) {
                writer.insert(feature, segTarget);
                // The append happened after the target's trees were built, so rebuild them over
                // the grown stores before adopting them.
                segTarget.reindex();
                adoptTrees(g, target, segTarget.kd, segTarget.rt);
            }
            publishRefreshed();
        } finally {
            lock.writeLock().unlock();
        }
    }

    /**
     * Retires a feature from both trees.
     *
     * <p>The delete is a tombstone in each tree, so no rebuild is involved and the cost stays
     * independent of segment size.
     *
     * @return true when a live feature was retired
     */
    public boolean delete(long featureId) {
        lock.writeLock().lock();
        try {
            Generation g = generation;
            for (int i = 0; i < g.segments().size(); i++) {
                GeoFeature found;
                try (SegmentTarget segTarget = SegmentTarget.open(g.segments().get(i), g, i)) {
                    found = segTarget.find(featureId);
                    if (found == null) {
                        continue;
                    }
                    writer.delete(found, segTarget);
                    segTarget.reindex();
                    adoptTrees(g, i, segTarget.kd, segTarget.rt);
                }
                publishRefreshed();
                return true;
            }
            return false;
        } finally {
            lock.writeLock().unlock();
        }
    }

    /**
     * Rewrites the generation after a write, recomputing per-segment counts and bounds.
     *
     * <p>The trees themselves are mutated in place, so this only rebuilds the metadata snapshot
     * that queries route on.
     */
    private void publishRefreshed() {
        Generation g = generation;
        List<SegmentLoader.SegmentMeta> metas = new ArrayList<>(g.segments().size());
        long total = 0;
        for (int i = 0; i < g.segments().size(); i++) {
            metas.add(refreshMeta(g.segments().get(i), g, i));
            total += metas.get(i).totalCount();
        }
        generation = new Generation(metas, g.kdTrees(), g.rTrees(), total);
        syncMetaFiles(metas);
    }

    /**
     * Swaps one segment's trees into the generation.
     *
     * <p>The trees handed over were built over the segment's writable stores inside a write
     * transaction, so they already reflect the appended or tombstoned rows.
     */
    private void adoptTrees(Generation g, int i, KDTreeIndex kd, RTreeIndex rt) {
        List<KDTreeIndex> kds = new ArrayList<>(g.kdTrees());
        List<RTreeIndex> rts = new ArrayList<>(g.rTrees());
        kds.set(i, kd);
        rts.set(i, rt);
        generation = new Generation(g.segments(), kds, rts, g.featureCount());
    }

    private SegmentLoader.SegmentMeta refreshMeta(SegmentLoader.SegmentMeta meta, Generation g, int i) {
        KDTreeIndex kd = g.kdTrees().get(i);
        RTreeIndex rt = g.rTrees().get(i);
        BBox bounds = boundsOf(kd, rt);
        return new SegmentLoader.SegmentMeta(meta.segmentId(), meta.directory(),
                kd.store().count(), rt.store().count(), bounds, meta.bytesOnDisk());
    }

    /** Rewrites the on-disk sidecars so a reopened index sees the post-write counts. */
    private void syncMetaFiles(List<SegmentLoader.SegmentMeta> metas) {
        for (SegmentLoader.SegmentMeta m : metas) {
            try {
                java.nio.file.Files.writeString(m.directory().resolve(SegmentLoader.META_FILE),
                        m.segmentId() + "\t" + m.pointCount() + "\t" + m.areaCount()
                                + "\t" + m.bounds().minX() + "\t" + m.bounds().minY() + "\t"
                                + m.bounds().maxX() + "\t" + m.bounds().maxY() + "\t"
                                + m.bytesOnDisk());
            } catch (IOException e) {
                throw new UncheckedIOException("cannot refresh segment metadata", e);
            }
        }
    }

    private static BBox boundsOf(KDTreeIndex kd, RTreeIndex rt) {
        BBox bounds = null;
        for (int row = 0; row < kd.size(); row++) {
            if (kd.isTombstone(row)) {
                continue;
            }
            BBox b = kd.store().bboxAt(row);
            bounds = bounds == null ? b : bounds.union(b);
        }
        for (int row = 0; row < rt.size(); row++) {
            if (rt.isTombstone(row)) {
                continue;
            }
            BBox b = rt.envelopeOf(row);
            bounds = bounds == null ? b : bounds.union(b);
        }
        return bounds == null ? new BBox(0, 0, 0, 0) : bounds;
    }

    /**
     * Rebuilds every segment from the current data and publishes the new generation atomically.
     *
     * <p>Queries running concurrently keep serving the previous generation until the swap, so they
     * never observe a half-built index.
     *
     * @return metadata of the rebuilt segments
     */
    public List<SegmentLoader.SegmentMeta> rebuild() {
        // Rebuild from the live rows of every segment. A feature lives in exactly one store (points
        // in the KD store, parcels in the R store), so collecting by id would otherwise duplicate
        // anything that was written to both by a transaction.
        List<GeoFeature> all = new ArrayList<>();
        java.util.Set<Long> seen = new java.util.HashSet<>();
        Generation g = generation;
        for (int i = 0; i < g.segments().size(); i++) {
            KDTreeIndex kd = g.kdTrees().get(i);
            kd.forEachLiveRow(row -> {
                GeoFeature f = kd.store().featureAt(row);
                if (seen.add(f.id())) {
                    all.add(f);
                }
            });
            RTreeIndex rt = g.rTrees().get(i);
            rt.forEachLiveRow(row -> {
                GeoFeature f = rt.store().featureAt(row);
                if (seen.add(f.id())) {
                    all.add(f);
                }
            });
        }
        List<SegmentLoader.SegmentMeta> metas;
        lock.writeLock().lock();
        try {
            metas = loader.load(all.iterator());
            List<KDTreeIndex> kd = new ArrayList<>(metas.size());
            List<RTreeIndex> rt = new ArrayList<>(metas.size());
            long total = 0;
            for (SegmentLoader.SegmentMeta meta : metas) {
                kd.add(meta.pointCount() > 0
                        ? new KDTreeIndex(SegmentStore.openReadOnly(meta.directory()
                        .resolve(SegmentLoader.POINT_FILE)))
                        : new KDTreeIndex(emptyStore(meta.directory().resolve(SegmentLoader.POINT_FILE))));
                rt.add(meta.areaCount() > 0
                        ? new RTreeIndex(SegmentStore.openReadOnly(meta.directory()
                        .resolve(SegmentLoader.AREA_FILE)))
                        : new RTreeIndex(emptyStore(meta.directory().resolve(SegmentLoader.AREA_FILE))));
                total += meta.totalCount();
            }
            generation = new Generation(metas, kd, rt, total);
            return metas;
        } finally {
            lock.writeLock().unlock();
        }
    }

    /**
     * Merges every segment into one, halving the segment count so later merges cost less.
     *
     * @return metadata of the merged segment
     */
    public SegmentLoader.SegmentMeta compactSegments() {
        lock.writeLock().lock();
        try {
            Generation g = generation;
            if (g.segments().size() < 2) {
                throw new IllegalStateException("need at least two segments to merge");
            }
            SegmentLoader.SegmentMeta merged = loader.merge(g.segments(), g.segments().size());
            KDTreeIndex kd = new KDTreeIndex(SegmentStore.openReadOnly(
                    merged.directory().resolve(SegmentLoader.POINT_FILE)));
            RTreeIndex rt = new RTreeIndex(SegmentStore.openReadOnly(
                    merged.directory().resolve(SegmentLoader.AREA_FILE)));
            generation = new Generation(List.of(merged), List.of(kd), List.of(rt),
                    merged.totalCount());
            return merged;
        } finally {
            lock.writeLock().unlock();
        }
    }

    /**
     * Writable view over one segment's stores, used as the transaction target.
     *
     * <p>The stores are opened read-write with capacity for one extra row so an insert can be
     * appended without rebuilding the segment. If the segment is already at capacity the insert
     * overflows into a freshly created segment instead, keeping the append-only invariant.
     */
    private static final class SegmentTarget implements IndexWriter.Target, AutoCloseable {
        KDTreeIndex kd;
        RTreeIndex rt;
        private final SegmentStore pointStore;
        private final SegmentStore areaStore;

        private SegmentTarget(SegmentLoader.SegmentMeta meta, KDTreeIndex kd, RTreeIndex rt,
                              SegmentStore pointStore, SegmentStore areaStore) {
            this.kd = kd;
            this.rt = rt;
            this.pointStore = pointStore;
            this.areaStore = areaStore;
        }

        /**
         * Opens the segment's stores for writing.
         *
         * <p>A segment created by the loader reserves exactly the rows it filled, so there is no
         * spare room to append into; such a segment is reopened with one extra row reserved.
         */
        static SegmentTarget open(SegmentLoader.SegmentMeta meta, Generation g, int i) {
            Path pointPath = meta.directory().resolve(SegmentLoader.POINT_FILE);
            Path areaPath = meta.directory().resolve(SegmentLoader.AREA_FILE);
            SegmentStore points = SegmentStore.reopen(pointPath);
            SegmentStore areas = SegmentStore.reopen(areaPath);
            // Build this target's own trees over the writable stores so tombstone flips land in the
            // segment file; the generation's trees are read-only views of the same bytes.
            KDTreeIndex kd = new KDTreeIndex(points);
            RTreeIndex rt = new RTreeIndex(areas);
            return new SegmentTarget(meta, kd, rt, points, areas);
        }

        @Override
        public boolean applyToKd(IndexWriter.Op op, GeoFeature feature) {
            return switch (op) {
                case INSERT, UPDATE -> {
                    pointStore.append(feature);
                    yield true;
                }
                case DELETE -> {
                    int row = rowOf(pointStore, feature.id());
                    yield row >= 0 && !pointStore.isTombstone(row) && kd.tombstone(row);
                }
            };
        }

        @Override
        public boolean applyToRTree(IndexWriter.Op op, GeoFeature feature) {
            return switch (op) {
                case INSERT, UPDATE -> {
                    areaStore.append(feature);
                    yield true;
                }
                case DELETE -> {
                    int row = rowOf(areaStore, feature.id());
                    yield row >= 0 && !areaStore.isTombstone(row) && rt.tombstone(row);
                }
            };
        }

        @Override
        public void undoKd(IndexWriter.Op op, GeoFeature feature) {
            if (op == IndexWriter.Op.DELETE) {
                int row = rowOf(pointStore, feature.id());
                if (row >= 0) {
                    kd.revive(row);
                }
            }
        }

        @Override
        public void undoRTree(IndexWriter.Op op, GeoFeature feature) {
            // A delete that reached the R tree only ever removed a tombstone, so undoing means
            // restoring the row to the same retired state the KD tree is already in.
            if (op == IndexWriter.Op.DELETE) {
                int row = rowOf(areaStore, feature.id());
                if (row >= 0) {
                    rt.tombstone(row);
                }
            }
        }

        @Override
        public void sync() {
            pointStore.flushHeader();
            areaStore.flushHeader();
            pointStore.sync();
            areaStore.sync();
        }

        GeoFeature find(long featureId) {
            int p = rowOf(pointStore, featureId);
            if (p >= 0 && !pointStore.isTombstone(p)) {
                return pointStore.featureAt(p);
            }
            int a = rowOf(areaStore, featureId);
            if (a >= 0 && !areaStore.isTombstone(a)) {
                return areaStore.featureAt(a);
            }
            return null;
        }

        /**
         * Rebuilds both trees over the current store contents.
         *
         * <p>Called once a write transaction has finished: an append or a tombstone changes what the
         * trees must cover, and the generation can only be published from indexes that already
         * reflect it.
         */
        void reindex() {
            pointStore.flushHeader();
            areaStore.flushHeader();
            kd = new KDTreeIndex(pointStore);
            rt = new RTreeIndex(areaStore);
        }

        @Override
        public void close() {
            pointStore.sync();
            areaStore.sync();
        }

        private static int rowOf(SegmentStore store, long id) {
            for (int i = 0; i < store.count(); i++) {
                if (store.idAt(i) == id && !store.isTombstone(i)) {
                    return i;
                }
            }
            return -1;
        }
    }

    @Override
    public void close() {
        lock.writeLock().lock();
        try {
            Generation g = generation;
            for (int i = 0; i < g.segmentCount(); i++) {
                g.kdTrees().get(i).store().close();
                g.rTrees().get(i).store().close();
            }
            executor.close();
            writer.close();
        } finally {
            lock.writeLock().unlock();
        }
    }
}

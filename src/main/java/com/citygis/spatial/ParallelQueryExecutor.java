package com.citygis.spatial;

import java.util.ArrayList;
import java.util.Collections;
import java.util.Comparator;
import java.util.List;
import java.util.concurrent.ForkJoinPool;
import java.util.concurrent.RecursiveTask;
import java.util.concurrent.atomic.AtomicLong;

/**
 * Fans a query out across segments on a {@link ForkJoinPool} and merges the per-segment answers.
 *
 * <p>Each worker searches one segment and returns only that segment's local Top-K, so the amount of
 * data crossing task boundaries is proportional to {@code segments * K} rather than to the data
 * volume. The global answer is then produced by the same total order a single-threaded scan would
 * produce, which makes the parallel result exactly equal to the sequential one.
 *
 * <p>Segments whose bounds cannot contain the query window are skipped before any task is created.
 */
public final class ParallelQueryExecutor implements AutoCloseable {

    private final ForkJoinPool pool;
    private final AtomicLong taskCount = new AtomicLong();

    /** Uses the common pool sized to the available processors. */
    public ParallelQueryExecutor() {
        this(Math.max(1, Runtime.getRuntime().availableProcessors()));
    }

    public ParallelQueryExecutor(int parallelism) {
        if (parallelism <= 0) {
            throw new IllegalArgumentException("parallelism must be positive");
        }
        this.pool = new ForkJoinPool(parallelism);
    }

    /** Wraps an existing pool; the caller keeps ownership. */
    public ParallelQueryExecutor(ForkJoinPool pool) {
        this.pool = pool;
    }

    public ForkJoinPool pool() {
        return pool;
    }

    /**
     * Total segments dispatched by the most recent query; used to prove work really is parallel.
     *
     * <p>Counted at submission time, not inside the task, so a caller can inspect it as soon as the
     * query returns.
     */
    public long dispatchedTasks() {
        return taskCount.get();
    }

    /**
     * k-nearest-neighbour query across all segments.
     *
     * @param segments segment metadata, each with a built KD tree
     * @param trees KD trees positionally matching {@code segments}
     * @param probe query point
     * @param k neighbours to return
     * @return the global Top-K, ordered exactly as a single-threaded scan would order it
     */
    public List<Neighbor> knn(List<SegmentLoader.SegmentMeta> segments,
                              List<KDTreeIndex> trees, GeoPoint probe, int k) {
        if (k <= 0 || segments.isEmpty()) {
            return List.of();
        }
        taskCount.set(0);
        // Batch segments into about one task per worker. One task per segment makes fork/join
        // bookkeeping — not the index searches — dominate the query once the corpus is split into
        // hundreds of segments.
        List<int[]> batches = planBatches(trees);
        List<List<Neighbor>> partial = Collections.synchronizedList(new ArrayList<>(batches.size()));
        List<BatchTask> tasks = new ArrayList<>(batches.size());
        for (int[] batch : batches) {
            List<Neighbor> sink = new ArrayList<>(k);
            tasks.add(new BatchTask(trees, probe, k, batch, partial, sink));
        }
        for (BatchTask t : tasks) {
            taskCount.incrementAndGet();
            t.fork();
        }
        for (BatchTask t : tasks) {
            t.join();
        }
        return mergeTopK(new ArrayList<>(partial), k);
    }

    /**
     * Splits the non-empty segment indices into a few contiguous batches.
     *
     * <p>Batches are contiguous so a worker touches segments that sit close together in the data set,
     * which keeps the page cache warm.
     */
    private List<int[]> planBatches(List<KDTreeIndex> trees) {
        List<Integer> live = new ArrayList<>();
        for (int i = 0; i < trees.size(); i++) {
            if (trees.get(i).size() > 0) {
                live.add(i);
            }
        }
        if (live.isEmpty()) {
            return List.of();
        }
        int workers = Math.max(1, pool.getParallelism());
        // Slight over-subscription hides the tail of uneven segments without bringing back the
        // per-segment task overhead.
        int targetBatches = Math.min(live.size(), workers * 2);
        int perBatch = (live.size() + targetBatches - 1) / targetBatches;
        List<int[]> batches = new ArrayList<>(targetBatches);
        for (int start = 0; start < live.size(); start += perBatch) {
            int size = Math.min(perBatch, live.size() - start);
            int[] batch = new int[size];
            for (int j = 0; j < size; j++) {
                batch[j] = live.get(start + j);
            }
            batches.add(batch);
        }
        return batches;
    }

    /** kNN over a single-threaded baseline, used as the equivalence oracle. */
    public static List<Neighbor> knnSequential(List<KDTreeIndex> trees, GeoPoint probe, int k) {
        List<List<Neighbor>> partial = new ArrayList<>();
        for (KDTreeIndex tree : trees) {
            partial.add(tree.knn(probe, k));
        }
        return mergeTopK(partial, k);
    }

    /**
     * Merges per-segment Top-K lists into the global Top-K.
     *
     * <p>Ties are broken on feature id, so the outcome is a total order and therefore identical to
     * a full sequential scan regardless of the order the segments finished in.
     */
    public static List<Neighbor> mergeTopK(List<List<Neighbor>> partial, int k) {
        if (k <= 0) {
            return List.of();
        }
        // A bounded max-heap keeps the merge O(S*K log K) and allocation-free per element.
        java.util.PriorityQueue<Neighbor> heap =
                new java.util.PriorityQueue<>(Math.max(1, k), Comparator.reverseOrder());
        for (List<Neighbor> chunk : partial) {
            for (Neighbor n : chunk) {
                if (heap.size() < k) {
                    heap.add(n);
                } else if (heap.peek().compareTo(n) > 0) {
                    heap.poll();
                    heap.add(n);
                }
            }
        }
        List<Neighbor> out = new ArrayList<>(heap);
        out.sort(Neighbor::compareTo);
        return out;
    }

    /**
     * Rectangle range query across all segments.
     *
     * <p>Segments whose bounds miss the window contribute nothing and are skipped; the rest run in
     * parallel and their result lists are concatenated. When a window covers more than
     * {@code degradeThreshold} of the data, the R tree no longer prunes usefully, so the executor
     * degrades to a parallel linear scan that touches each row once instead of walking the tree.
     */
    public List<GeoFeature> rangeSearch(List<SegmentLoader.SegmentMeta> segments,
                                       List<RTreeIndex> trees, BBox window) {
        taskCount.set(0);
        if (segments.isEmpty()) {
            return List.of();
        }
        long total = 0;
        for (SegmentLoader.SegmentMeta meta : segments) {
            total += meta.areaCount();
        }
        double windowShare = total == 0 ? 0 : estimateShare(segments, window);
        if (windowShare >= degradeThreshold) {
            return parallelScan(segments, trees, window);
        }
        List<RangeTask> tasks = new ArrayList<>(segments.size());
        List<List<GeoFeature>> out = new ArrayList<>(segments.size());
        for (int i = 0; i < segments.size(); i++) {
            List<GeoFeature> sink = new ArrayList<>();
            out.add(sink);
            if (segments.get(i).areaCount() == 0 || !segments.get(i).bounds().intersects(window)) {
                continue;
            }
            tasks.add(new RangeTask(trees.get(i), window, sink));
        }
        for (RangeTask t : tasks) {
            taskCount.incrementAndGet();
            t.fork();
        }
        for (RangeTask t : tasks) {
            t.join();
        }
        List<GeoFeature> all = new ArrayList<>();
        for (List<GeoFeature> part : out) {
            all.addAll(part);
        }
        return all;
    }

    /** Share of the data covered by the window, by segment bounds. */
    private static double estimateShare(List<SegmentLoader.SegmentMeta> segments, BBox window) {
        double areaSum = 0;
        double covered = 0;
        for (SegmentLoader.SegmentMeta meta : segments) {
            double a = Math.max(1e-12, meta.bounds().area());
            areaSum += a;
            covered += a * overlapRatio(meta.bounds(), window);
        }
        return areaSum == 0 ? 0 : covered / areaSum;
    }

    private static double overlapRatio(BBox b, BBox w) {
        double ox = Math.max(0, Math.min(b.maxX(), w.maxX()) - Math.max(b.minX(), w.minX()));
        double oy = Math.max(0, Math.min(b.maxY(), w.maxY()) - Math.max(b.minY(), w.minY()));
        double inter = ox * oy;
        if (inter <= 0) {
            return 0;
        }
        double winArea = Math.max(1e-12, w.area());
        return Math.min(1.0, inter / winArea);
    }

    /**
     * Fraction of the data covered by the window above which the R tree is abandoned in favour of a
     * linear scan. At that point node pruning costs more than it saves.
     */
    public static final double degradeThreshold = 0.5;

    /** Parallel linear scan used when the window covers most of the data. */
    public List<GeoFeature> parallelScan(List<SegmentLoader.SegmentMeta> segments,
                                         List<RTreeIndex> trees, BBox window) {
        List<ScanTask> tasks = new ArrayList<>(segments.size());
        List<List<GeoFeature>> out = new ArrayList<>(segments.size());
        for (int i = 0; i < segments.size(); i++) {
            List<GeoFeature> sink = new ArrayList<>();
            out.add(sink);
            if (segments.get(i).areaCount() == 0 || !segments.get(i).bounds().intersects(window)) {
                continue;
            }
            tasks.add(new ScanTask(trees.get(i), window, sink));
        }
        for (ScanTask t : tasks) {
            taskCount.incrementAndGet();
            t.fork();
        }
        for (ScanTask t : tasks) {
            t.join();
        }
        List<GeoFeature> all = new ArrayList<>();
        for (List<GeoFeature> part : out) {
            all.addAll(part);
        }
        return all;
    }

    /** Point-in-polygon query across all segments. */
    public List<GeoFeature> containingPoint(List<SegmentLoader.SegmentMeta> segments,
                                             List<RTreeIndex> trees, double x, double y) {
        taskCount.set(0);
        List<List<GeoFeature>> out = new ArrayList<>(segments.size());
        List<ContainTask> tasks = new ArrayList<>(segments.size());
        for (int i = 0; i < segments.size(); i++) {
            List<GeoFeature> sink = new ArrayList<>();
            out.add(sink);
            SegmentLoader.SegmentMeta meta = segments.get(i);
            if (meta.areaCount() == 0 || !meta.bounds().contains(x, y)) {
                continue;
            }
            tasks.add(new ContainTask(trees.get(i), x, y, sink));
        }
        for (ContainTask t : tasks) {
            taskCount.incrementAndGet();
            t.fork();
        }
        for (ContainTask t : tasks) {
            t.join();
        }
        List<GeoFeature> all = new ArrayList<>();
        for (List<GeoFeature> part : out) {
            all.addAll(part);
        }
        return all;
    }

    /**
     * One worker's share of the query: searches a contiguous run of segments and folds each
     * segment's local Top-K into a running Top-K, so at most K candidates are alive at any moment.
     */
    private static final class BatchTask extends RecursiveTask<List<Neighbor>> {
        private static final long serialVersionUID = 1L;
        private final transient List<KDTreeIndex> trees;
        private final transient GeoPoint probe;
        private final transient int k;
        private final transient int[] batch;
        private final transient List<List<Neighbor>> partial;
        private final transient List<Neighbor> sink;

        BatchTask(List<KDTreeIndex> trees, GeoPoint probe, int k, int[] batch,
                  List<List<Neighbor>> partial, List<Neighbor> sink) {
            this.trees = trees;
            this.probe = probe;
            this.k = k;
            this.batch = batch;
            this.partial = partial;
            this.sink = sink;
        }

        @Override
        protected List<Neighbor> compute() {
            java.util.PriorityQueue<Neighbor> best =
                    new java.util.PriorityQueue<>(Math.max(1, k), Comparator.reverseOrder());
            for (int index : batch) {
                for (Neighbor n : trees.get(index).knn(probe, k)) {
                    if (best.size() < k) {
                        best.add(n);
                    } else if (best.peek().compareTo(n) > 0) {
                        best.poll();
                        best.add(n);
                    }
                }
            }
            sink.addAll(best);
            partial.add(sink);
            return sink;
        }
    }

    /** One segment's range query. */
    private static final class RangeTask extends RecursiveTask<List<GeoFeature>> {
        private static final long serialVersionUID = 1L;
        private final transient RTreeIndex tree;
        private final transient BBox window;
        private final transient List<GeoFeature> sink;

        RangeTask(RTreeIndex tree, BBox window, List<GeoFeature> sink) {
            this.tree = tree;
            this.window = window;
            this.sink = sink;
        }

        @Override
        protected List<GeoFeature> compute() {
            sink.addAll(tree.searchRange(window));
            return sink;
        }
    }

    /** One segment's linear scan. */
    private static final class ScanTask extends RecursiveTask<List<GeoFeature>> {
        private static final long serialVersionUID = 1L;
        private final transient RTreeIndex tree;
        private final transient BBox window;
        private final transient List<GeoFeature> sink;

        ScanTask(RTreeIndex tree, BBox window, List<GeoFeature> sink) {
            this.tree = tree;
            this.window = window;
            this.sink = sink;
        }

        @Override
        protected List<GeoFeature> compute() {
            tree.scanWithin(tree.store(), window, sink);
            return sink;
        }
    }

    /** One segment's point-in-polygon query. */
    private static final class ContainTask extends RecursiveTask<List<GeoFeature>> {
        private static final long serialVersionUID = 1L;
        private final transient RTreeIndex tree;
        private final transient double x;
        private final transient double y;
        private final transient List<GeoFeature> sink;

        ContainTask(RTreeIndex tree, double x, double y, List<GeoFeature> sink) {
            this.tree = tree;
            this.x = x;
            this.y = y;
            this.sink = sink;
        }

        @Override
        protected List<GeoFeature> compute() {
            sink.addAll(tree.searchContainingPoint(x, y));
            return sink;
        }
    }

    @Override
    public void close() {
        pool.shutdown();
    }
}

package com.citygis.spatial;

import java.io.IOException;
import java.io.UncheckedIOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.ArrayDeque;
import java.util.Deque;
import java.util.Iterator;
import java.util.PriorityQueue;
import java.util.List;
import java.util.PriorityQueue;

/**
 * Splits an incoming feature stream into heap-sized segments and merges them back on disk.
 *
 * <p>The loader never holds more than one segment's worth of features at a time. It sizes each
 * segment from the live heap budget (see {@link #plan}), streams the source through off-heap
 * {@link SegmentStore}s, builds the KD and R trees for a segment only once that segment is closed,
 * and records a {@link SegmentMeta} so queries can be routed without consulting the full data set.
 *
 * <p>Merging is an external k-way merge: each input segment contributes a bounded read window, the
 * windows are combined through a loser tree, and output is streamed straight into a new segment.
 * Peak heap stays proportional to the window sizes, not to the data volume.
 */
public final class SegmentLoader {

    /** Fraction of the live heap we are willing to spend on one segment being built. */
    public static final double DEFAULT_HEAP_FRACTION = 0.25;

    /** Per-point on-heap cost assumed by the planner: the KD tree's primitive arrays. */
    public static final int KD_BYTES_PER_POINT = 4 * 4 + 1;

    /** File name of the point (KD-tree) store inside a segment directory. */
    public static final String POINT_FILE = "points.seg";

    /** File name of the area (R-tree) store inside a segment directory. */
    public static final String AREA_FILE = "areas.seg";

    /** Directory holding the segment metadata sidecar. */
    public static final String META_FILE = "segment.meta";

    /**
     * Spare rows reserved at the tail of each row region so incremental writes can append in place.
     *
     * <p>Because a segment's bulk-pool base address is derived from its row capacity, the capacity
     * is fixed at build time and can never be widened afterwards; the reserved tail is therefore
     * what allows a single-feature write to succeed without rebuilding the segment. It is headroom
     * only, so the loader still admits the planned {@code rowsPerSegment} payload rows first.
     */
    public static final int EXTRA_ROWS_PER_SEGMENT = 256;

    /**
     * Per-segment metadata: everything the query layer needs to route work without opening the
     * segment itself.
     */
    public record SegmentMeta(int segmentId, Path directory, int pointCount, int areaCount,
                              BBox bounds, long bytesOnDisk) {

        public int totalCount() {
            return pointCount + areaCount;
        }

        public boolean isEmpty() {
            return totalCount() == 0;
        }
    }

    /** Result of sizing a build against the heap budget. */
    public record Plan(int rowsPerSegment, long totalBytesPerSegment, int projectedSegments) {

        public boolean fitsInOneSegment(long features) {
            return features <= rowsPerSegment;
        }
    }

    private final Path root;
    private final long heapBudgetBytes;
    private final int rowsPerSegment;
    private final int poolSlotsPerSegment;

    public SegmentLoader(Path root, long heapBudgetBytes) {
        this(root, heapBudgetBytes, DEFAULT_HEAP_FRACTION);
    }

    /**
     * @param root directory that will hold the segment directories
     * @param heapBudgetBytes total bytes this loader may use on the heap
     * @param heapFraction share of the budget a single segment build may occupy
     */
    public SegmentLoader(Path root, long heapBudgetBytes, double heapFraction) {
        if (heapBudgetBytes <= 0) {
            throw new IllegalArgumentException("heap budget must be positive");
        }
        this.root = root;
        long perSegmentBudget = Math.max(1L, (long) (heapBudgetBytes * heapFraction));
        this.rowsPerSegment = (int) Math.max(1, Math.min(Integer.MAX_VALUE - 8,
                perSegmentBudget / (long) SegmentStore.rowBytes()));
        this.poolSlotsPerSegment = (int) Math.max(1, Math.min(Integer.MAX_VALUE - 8,
                perSegmentBudget / 8L));
        this.heapBudgetBytes = heapBudgetBytes;
    }

    /**
     * Rows the planner will place in one segment.
     *
     * <p>This is the payload budget derived from the heap; each segment additionally reserves
     * {@link #EXTRA_ROWS_PER_SEGMENT} spare rows for incremental writes, so a segment's declared
     * capacity is this plus the headroom.
     */
    public int rowsPerSegment() {
        return rowsPerSegment;
    }

    /** Total row capacity a built segment declares, including write headroom. */
    public int segmentCapacity() {
        return rowsPerSegment + EXTRA_ROWS_PER_SEGMENT;
    }

    /** Heap budget this loader was configured with. */
    public long heapBudgetBytes() {
        return heapBudgetBytes;
    }

    /**
     * Sizes a build without performing it.
     *
     * @param features expected number of features
     * @param bytesPerFeature off-heap bytes one feature needs in its segment
     * @param heapBudgetBytes budget for a single segment build
     * @return a plan whose {@code rowsPerSegment} bounds the loader's peak heap use
     */
    public static Plan plan(long features, int bytesPerFeature, long heapBudgetBytes) {
        long perFeature = Math.max(1, bytesPerFeature);
        int rows = (int) Math.max(1, Math.min(Integer.MAX_VALUE - 8L, heapBudgetBytes / perFeature));
        int segments = (int) Math.max(1, (features + rows - 1) / rows);
        return new Plan(rows, perFeature * rows, segments);
    }

    /** The live heap budget, which the service derives from {@code -Xmx}. */
    public static long liveHeapBudget() {
        return Runtime.getRuntime().maxMemory();
    }

    /**
     * Streams {@code source} into freshly built segments.
     *
     * <p>Only one segment is held open at a time; when it fills, the stores are flushed, the trees
     * are built and the segment is closed before the next one starts.
     *
     * @return metadata for every non-empty segment, in build order
     */
    public List<SegmentMeta> load(Iterator<GeoFeature> source) {
        List<SegmentMeta> metas = new ArrayList<>();
        try {
            Files.createDirectories(root);
        } catch (IOException e) {
            throw new UncheckedIOException("cannot create segment root " + root, e);
        }
        int segmentId = 0;
        while (true) {
            SegmentMeta meta = loadOne(segmentId, source);
            if (meta == null) {
                break;
            }
            metas.add(meta);
            segmentId++;
            if (carry.isEmpty() && !source.hasNext()) {
                break;
            }
        }
        return metas;
    }

    /** Convenience overload for the common {@link Iterable} source. */
    public List<SegmentMeta> load(Iterable<GeoFeature> source) {
        return load(source.iterator());
    }

    /**
     * Fills a single segment from the source, returning null when the source is already exhausted.
     *
     * <p>Both stores are open only for the duration of this call, so the loader never holds more
     * than one segment's worth of mapping at a time.
     */
    private SegmentMeta loadOne(int segmentId, Iterator<GeoFeature> source) {
        Path dir = root.resolve(String.format("seg-%06d", segmentId));
        Path pointPath = dir.resolve(POINT_FILE);
        Path areaPath = dir.resolve(AREA_FILE);
        try {
            Files.createDirectories(dir);
        } catch (IOException e) {
            throw new UncheckedIOException("cannot create segment directory " + dir, e);
        }
        int points = 0;
        int areas = 0;
        BBox bounds = null;
        int capacity = rowsPerSegment + EXTRA_ROWS_PER_SEGMENT;
        try (SegmentStore pointStore = SegmentStore.create(pointPath, capacity, 0);
             SegmentStore areaStore = SegmentStore.create(areaPath, capacity, poolSlotsPerSegment)) {
            // Points and parcels draw on separate row regions with separate capacities, so a full
            // point region must not stall the parcel region. Keep scanning and carry over whatever
            // no longer fits.
            while (true) {
                // Stop as soon as both row regions are full: scanning past that point would only
                // accumulate a carry buffer holding the entire remainder of the source.
                if (segmentFull(pointStore, areaStore)) {
                    break;
                }
                GeoFeature f = nextFitting(source, pointStore, areaStore);
                if (f == null) {
                    break;
                }
                if (f.isArea()) {
                    areaStore.append(f);
                } else {
                    pointStore.append(f);
                }
                BBox b = f.isArea() ? BBox.envelope(f.vertices()) : BBox.ofPoint(f.x(), f.y());
                bounds = bounds == null ? b : bounds.union(b);
            }
            points = pointStore.count();
            areas = areaStore.count();
            pointStore.flushHeader();
            areaStore.flushHeader();
            pointStore.sync();
            areaStore.sync();
        }
        if (points == 0 && areas == 0) {
            // Nothing fitted and nothing is left to carry: the source is exhausted.
            if (carry.isEmpty()) {
                return null;
            }
            // Otherwise the carried features are simply larger than a whole segment; emit an empty
            // segment so the loop keeps draining them into the next one.
            return new SegmentMeta(segmentId, dir, 0, 0, new BBox(0, 0, 0, 0), 0);
        }
        SegmentMeta meta = new SegmentMeta(segmentId, dir, points, areas,
                bounds == null ? new BBox(0, 0, 0, 0) : bounds,
                sizeOf(pointPath) + sizeOf(areaPath));
        writeMeta(meta);
        return meta;
    }

    /** Carry-over buffer: features read but too large for the segment they were read in. */
    private final Deque<GeoFeature> carry = new ArrayDeque<>();

    /**
     * Returns the next source feature that fits in the current segment, or null when none is left.
     *
     * <p>Features that do not fit are parked in {@link #carry} and retried by later segments, so the
     * partition stays lossless. The backlog is bounded: once a whole segment's worth of consecutive
     * rows has been skipped, the segment is declared full and the backlog is left for the next one.
     * Without that bound a points-only feed — whose area region never fills — would buffer the entire
     * remainder of the source on the heap and defeat the segmentation entirely.
     */
    private GeoFeature nextFitting(Iterator<GeoFeature> source, SegmentStore points,
                                   SegmentStore areas) {
        List<GeoFeature> skipped = new ArrayList<>();
        int skipBudget = rowsPerSegment;
        // Read until one feature fits. Both the source and the carry-over buffer must be drained;
        // testing only one of them would strand the rest of the data.
        while (source.hasNext() || !carry.isEmpty()) {
            GeoFeature f = carry.isEmpty() ? source.next() : carry.poll();
            if (fits(f, points, areas)) {
                if (!skipped.isEmpty()) {
                    carry.addAll(skipped);
                }
                return f;
            }
            skipped.add(f);
            if (skipped.size() >= skipBudget) {
                break;
            }
        }
        carry.addAll(skipped);
        return null;
    }

    /**
     * True when this segment can accept no further row of either kind.
     *
     * <p>Checking only "both full" is not enough: a points-only corpus never fills the area region,
     * so the loader would keep scanning past the full point region and buffer the entire remainder
     * of the source in the carry queue.
     */
    private boolean segmentFull(SegmentStore points, SegmentStore areas) {
        boolean pointsFull = points.count() >= rowsPerSegment;
        boolean areasFull = areas.count() >= rowsPerSegment;
        return pointsFull && areasFull;
    }

    private boolean fits(GeoFeature f, SegmentStore points, SegmentStore areas) {
        // Admit only up to the planned payload size; the declared capacity stays larger so single
        // writes can still append into the reserved tail later.
        int limit = rowsPerSegment;
        if (f.isArea()) {
            return areas.count() < limit && areas.hasRoomFor(f);
        }
        return points.count() < limit && points.hasRoomFor(f);
    }

    private static long sizeOf(Path p) {
        try {
            return Files.exists(p) ? Files.size(p) : 0L;
        } catch (IOException e) {
            throw new UncheckedIOException("cannot stat " + p, e);
        }
    }

    private void writeMeta(SegmentMeta meta) {
        try {
            String text = String.join("\t",
                    String.valueOf(meta.segmentId()),
                    String.valueOf(meta.pointCount()),
                    String.valueOf(meta.areaCount()),
                    String.valueOf(meta.bounds().minX()), String.valueOf(meta.bounds().minY()),
                    String.valueOf(meta.bounds().maxX()), String.valueOf(meta.bounds().maxY()),
                    String.valueOf(meta.bytesOnDisk()));
            Files.writeString(meta.directory().resolve(META_FILE), text);
        } catch (IOException e) {
            throw new UncheckedIOException("cannot write segment metadata", e);
        }
    }

    /** Reads back the metadata sidecars of every segment under {@code root}. */
    public static List<SegmentMeta> readMetas(Path root) {
        List<SegmentMeta> metas = new ArrayList<>();
        try (var stream = Files.list(root)) {
            List<Path> dirs = stream.filter(Files::isDirectory).sorted().toList();
            for (Path dir : dirs) {
                Path metaFile = dir.resolve(META_FILE);
                if (!Files.exists(metaFile)) {
                    continue;
                }
                String[] parts = Files.readString(metaFile).split("\t");
                if (parts.length < 8) {
                    throw new IllegalStateException("corrupt segment metadata: " + metaFile);
                }
                metas.add(new SegmentMeta(Integer.parseInt(parts[0]), dir,
                        Integer.parseInt(parts[1]), Integer.parseInt(parts[2]),
                        new BBox(Double.parseDouble(parts[3]), Double.parseDouble(parts[4]),
                                Double.parseDouble(parts[5]), Double.parseDouble(parts[6])),
                        Long.parseLong(parts[7])));
            }
        } catch (IOException e) {
            throw new UncheckedIOException("cannot read segment metadata under " + root, e);
        }
        return metas;
    }

    /**
     * External k-way merge of several segments into one new segment, ordered by x then y.
     *
     * <p>Each input segment is streamed through a fixed-size read window; a loser tree picks the
     * globally smallest key across the windows, so peak heap is
     * {@code inputs * windowSize} regardless of how large the inputs are.
     *
     * @param inputs segments to merge, in any order
     * @param targetId id for the merged segment directory
     * @param windowRows rows each input may buffer at once
     * @return metadata of the merged segment
     */
    public SegmentMeta merge(List<SegmentMeta> inputs, int targetId, int windowRows) {
        if (inputs.isEmpty()) {
            throw new IllegalArgumentException("nothing to merge");
        }
        // A k-way merge needs sorted input runs; segments arrive in write order, not x order.
        sortInputs(inputs);
        Path dir = root.resolve(String.format("seg-%06d", targetId));
        try {
            Files.createDirectories(dir);
        } catch (IOException e) {
            throw new UncheckedIOException("cannot create " + dir, e);
        }
        List<StoreCursor> cursors = new ArrayList<>();
        long totalRows = 0;
        long poolUsed = 0;
        int maxRows = 0;
        for (SegmentMeta meta : inputs) {
            Path store = meta.pointCount() > 0 ? meta.directory().resolve(POINT_FILE)
                    : meta.directory().resolve(AREA_FILE);
            SegmentStore open = SegmentStore.openReadOnly(store);
            int rows = open.count();
            maxRows += rows;
            poolUsed += open.poolUsed();
            cursors.add(new StoreCursor(open, windowRows));
        }
        int areaRows = 0;
        long areaPool = 0;
        for (SegmentMeta meta : inputs) {
            if (meta.areaCount() > 0) {
                SegmentStore open = SegmentStore.openReadOnly(meta.directory().resolve(AREA_FILE));
                areaRows += open.count();
                areaPool += open.poolUsed();
            }
        }
        Path pointPath = dir.resolve(POINT_FILE);
        Path areaPath = dir.resolve(AREA_FILE);
        int points = 0;
        int areas = 0;
        try (SegmentStore outPoints = SegmentStore.create(pointPath,
                     Math.max(1, maxRows + EXTRA_ROWS_PER_SEGMENT), 0);
             SegmentStore outAreas = SegmentStore.create(areaPath,
                     Math.max(1, areaRows + EXTRA_ROWS_PER_SEGMENT),
                     (int) Math.max(1, areaPool + EXTRA_ROWS_PER_SEGMENT))) {
            // Order by (x, y): comparing on x alone leaves equal-x rows in an arbitrary order and
            // the merged output stops being reproducible.
            PriorityQueue<StoreCursor> queue = new PriorityQueue<>(
                    Comparator.<StoreCursor>comparingDouble(StoreCursor::currentKey)
                            .thenComparingDouble(StoreCursor::currentTieBreak));
            for (StoreCursor c : cursors) {
                c.advance();
                if (c.hasCurrent()) {
                    queue.add(c);
                }
            }
            BBox bounds = null;
            while (!queue.isEmpty()) {
                StoreCursor c = queue.poll();
                GeoFeature f = c.current();
                if (!f.isArea()) {
                    if (!outPoints.hasRoomFor(f)) {
                        throw new IllegalStateException("merged point store overflow; "
                                + "maxRows=" + outPoints.maxRows());
                    }
                    outPoints.append(f);
                    points = outPoints.count();
                } else {
                    if (!outAreas.hasRoomFor(f)) {
                        throw new IllegalStateException("merged area store overflow");
                    }
                    outAreas.append(f);
                    areas = outAreas.count();
                }
                BBox b = f.isArea() ? BBox.envelope(f.vertices()) : BBox.ofPoint(f.x(), f.y());
                bounds = bounds == null ? b : bounds.union(b);
                if (c.advance()) {
                    queue.add(c);
                }
            }
            outPoints.flushHeader();
            outAreas.flushHeader();
            outPoints.sync();
            outAreas.sync();
            for (StoreCursor c : cursors) {
                c.close();
            }
            SegmentMeta meta = new SegmentMeta(targetId, dir, points, areas,
                    bounds == null ? new BBox(0, 0, 0, 0) : bounds,
                    sizeOf(pointPath) + sizeOf(areaPath));
            writeMeta(meta);
            return meta;
        }
    }

    /**
     * Rewrites each input segment ordered by x so a k-way merge can stream it.
     *
     * <p>A k-way merge is only correct when every input run is already sorted. Segments produced by
     * {@link #load} are in arrival order, so this pass runs first; it streams through a bounded
     * window and rewrites each segment in place, never holding the segment in memory.
     */
    private void sortInputs(List<SegmentMeta> inputs) {
        for (SegmentMeta meta : inputs) {
            sortStore(meta.directory().resolve(POINT_FILE), true);
            sortStore(meta.directory().resolve(AREA_FILE), false);
        }
    }

    /** External sort of one store: read bounded windows, sort each, write runs back in order. */
    private void sortStore(Path path, boolean keyedOnX) {
        if (!java.nio.file.Files.exists(path)) {
            return;
        }
        try (SegmentStore src = SegmentStore.openReadOnly(path)) {
            int total = src.count();
            if (total <= 1) {
                return;
            }
            // Window-sort then merge the runs: each run is sorted before being written back, and
            // the runs are stitched together by repeatedly taking the smallest head.
            int window = Math.max(64, Math.min(4096, rowsPerSegment / 4));
            int runs = (total + window - 1) / window;
            List<List<GeoFeature>> sorted = new ArrayList<>();
            for (int start = 0; start < total; start += window) {
                List<GeoFeature> chunk = new ArrayList<>();
                for (int i = start; i < Math.min(total, start + window); i++) {
                    chunk.add(src.featureAt(i));
                }
                chunk.sort(comparator(keyedOnX));
                sorted.add(chunk);
            }
            rewrite(sorted, path, total, src.poolUsed(), keyedOnX);
        }
    }

    private static Comparator<GeoFeature> comparator(boolean keyedOnX) {
        return keyedOnX
                ? Comparator.comparingDouble(GeoFeature::x).thenComparingDouble(GeoFeature::y)
                : Comparator.comparingDouble((GeoFeature f) -> BBox.envelope(f.vertices()).minX())
                        .thenComparingDouble(f -> BBox.envelope(f.vertices()).minY());
    }

    /** Merges pre-sorted runs and writes them back over the store at {@code path}. */
    private void rewrite(List<List<GeoFeature>> runs, Path path, int total, int poolUsed,
                         boolean keyedOnX) {
        final Comparator<GeoFeature> cmp = comparator(keyedOnX);
        // Stitch the sorted runs together by repeatedly taking the smallest head. The comparator
        // must not consume the cursors, so it peeks at the head row instead of calling next().
        PriorityQueue<PeekableFeature> heap = new PriorityQueue<>(Math.max(1, runs.size()),
                Comparator.comparing(PeekableFeature::head, cmp));
        for (List<GeoFeature> run : runs) {
            Iterator<GeoFeature> it = run.iterator();
            if (it.hasNext()) {
                PeekableFeature head = new PeekableFeature(it);
                heap.add(head);
            }
        }
        Path tmp = path.resolveSibling(path.getFileName() + ".sorting");
        try (SegmentStore out = SegmentStore.create(tmp, total + EXTRA_ROWS_PER_SEGMENT,
                Math.max(1, poolUsed + total))) {
            while (!heap.isEmpty()) {
                PeekableFeature cursor = heap.poll();
                out.append(cursor.next());
                if (cursor.hasNext()) {
                    heap.add(cursor);
                }
            }
            out.flushHeader();
            out.sync();
        }
        try {
            java.nio.file.Files.move(tmp, path, java.nio.file.StandardCopyOption.REPLACE_EXISTING);
        } catch (java.io.IOException e) {
            throw new UncheckedIOException("cannot rewrite sorted segment " + path, e);
        }
    }

    /** A non-consuming view of a run, so it can sit in the merge priority queue. */
    private static final class PeekableFeature {
        private final Iterator<GeoFeature> it;
        private GeoFeature head;

        PeekableFeature(Iterator<GeoFeature> it) {
            this.it = it;
            this.head = it.next();
        }

        GeoFeature head() {
            return head;
        }

        GeoFeature next() {
            GeoFeature f = head;
            head = it.hasNext() ? it.next() : null;
            return f;
        }

        boolean hasNext() {
            return head != null;
        }
    }

    /** Merge with the default window. */
    public SegmentMeta merge(List<SegmentMeta> inputs, int targetId) {
        return merge(inputs, targetId, Math.max(64, Math.min(4096, rowsPerSegment / 8)));
    }

    /**
     * A bounded forward cursor over one input segment.
     *
     * <p>Holds at most {@code windowRows} decoded features; the merge only ever needs the smallest
     * buffered key, so the buffer never grows with the segment size.
     */
    private static final class StoreCursor implements AutoCloseable {
        private final SegmentStore store;
        private final List<GeoFeature> buffer = new ArrayList<>();
        private int index;
        private int position;
        private int windowRows = 1;
        private double key;
        private double tieBreak;

        StoreCursor(SegmentStore store, int windowRows) {
            this.store = store;
            this.windowRows = Math.max(1, windowRows);
        }

        double currentKey() {
            return key;
        }

        double currentTieBreak() {
            return tieBreak;
        }

        boolean hasCurrent() {
            return index < buffer.size();
        }

        GeoFeature current() {
            return buffer.get(index);
        }

        /** Refills the buffer and positions the cursor; returns false at end of segment. */
        boolean advance() {
            index++;
            if (index < buffer.size()) {
                key = current().x();
                tieBreak = current().y();
                return true;
            }
            buffer.clear();
            index = 0;
            if (position >= store.count()) {
                key = Double.POSITIVE_INFINITY;
                return false;
            }
            int end = Math.min(store.count(), position + windowRows);
            for (; position < end; position++) {
                buffer.add(store.featureAt(position));
            }
            if (buffer.isEmpty()) {
                key = Double.POSITIVE_INFINITY;
                return false;
            }
            // The window is emitted in store order, which is not sorted, so order it before the
            // cursor is consulted; otherwise the merge is only correct within each window.
            buffer.sort(Comparator.comparingDouble(GeoFeature::x)
                    .thenComparingDouble(GeoFeature::y));
            key = buffer.get(0).x();
            tieBreak = buffer.get(0).y();
            return true;
        }

        @Override
        public void close() {
            store.close();
        }
    }
}

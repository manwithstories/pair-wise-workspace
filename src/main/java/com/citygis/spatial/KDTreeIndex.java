package com.citygis.spatial;

import java.util.ArrayDeque;
import java.util.ArrayList;
import java.util.Deque;
import java.util.Arrays;
import java.util.Comparator;
import java.util.List;
import java.util.PriorityQueue;
import java.util.function.Consumer;
import java.util.function.IntConsumer;

/**
 * Off-heap KD tree over the point features of a single {@link SegmentStore}.
 *
 * <p>The tree keeps its shape in three parallel int arrays — row index, left/right child links and
 * the split axis — so the resident heap cost is a small constant per point and never grows with the
 * segment size. Coordinates are read straight from the mapped {@link SegmentStore}, and the
 * tombstone bitmap lives in the off-heap row flags, so deleting a point is a single byte store with
 * no rebuild.
 *
 * <p>Search is an iterative traversal holding a bounded max-heap of the K best hits so far. Subtrees
 * are pruned whenever the squared distance to the split plane exceeds the current K-th best
 * distance, making cost proportional to the neighbourhood rather than the segment size.
 */
public final class KDTreeIndex implements AutoCloseable {

    /** Value of a child link pointing at "no child". */
    private static final int NO_CHILD = -1;

    private final SegmentStore store;
    private int size;

    private final int[] slotToRow;
    private final int[] leftChild;
    private final int[] rightChild;
    private final byte[] splitAxis;

    private int tombstones;

    public KDTreeIndex(SegmentStore store) {
        this.store = store;
        this.size = store.count();
        // Rows may already carry tombstones (after a delete, or when reopening a segment), so the
        // live/tombstone split has to be derived from the store rather than assumed to be zero.
        int dead = 0;
        for (int i = 0; i < size; i++) {
            if (store.isTombstone(i)) {
                dead++;
            }
        }
        this.tombstones = dead;
        this.slotToRow = new int[size];
        this.leftChild = new int[size];
        this.rightChild = new int[size];
        this.splitAxis = new byte[size];
        Arrays.fill(leftChild, NO_CHILD);
        Arrays.fill(rightChild, NO_CHILD);
        // Identity permutation: slot i holds row i until the builder permutes it in place.
        for (int i = 0; i < size; i++) {
            slotToRow[i] = i;
        }
        buildRoot = buildRange(0, size);
    }

    public int size() {
        return size;
    }

    public int tombstoneCount() {
        return tombstones;
    }

    public int liveCount() {
        return size - tombstones;
    }

    public boolean isTombstone(int row) {
        return store.isTombstone(row);
    }

    public SegmentStore store() {
        return store;
    }

    /**
     * Marks a row deleted (lazy tombstone).
     *
     * @return true when this call retired a previously live row
     */
    public boolean tombstone(int row) {
        if (row < 0 || row >= size) {
            throw new IndexOutOfBoundsException("row " + row + " outside [0," + size + ")");
        }
        if (store.tombstone(row)) {
            tombstones++;
            return true;
        }
        return false;
    }

    /** Un-retires a row. */
    public boolean revive(int row) {
        if (store.revive(row)) {
            tombstones--;
            return true;
        }
        return false;
    }

    /**
     * Lazily merges tombstones away by rebuilding the tree over the surviving rows.
     *
     * <p>Called when tombstones exceed {@code thresholdFraction} of the segment so that repeated
     * delete cycles cannot degrade search performance; ordinary deletes never trigger it.
     */
    private static int tombstoneThreshold(int size, double fraction) {
        return Math.max(1, (int) Math.ceil(size * fraction));
    }

    /** Compact when the tombstone ratio exceeds {@code thresholdFraction}. */
    public void compactIfNeeded(double thresholdFraction) {
        if (tombstones > tombstoneThreshold(size, thresholdFraction)) {
            compact();
        }
    }

    /**
     * Rebuilds the tree over live rows only, releasing tombstoned slots.
     *
     * <p>Compaction is optional and rate-limited: it trades a rebuild for the reclaimed slots, so
     * callers normally run it only after the tombstone ratio crosses a threshold.
     */
    public void compact() {
        int n = 0;
        for (int i = 0; i < size; i++) {
            if (!store.isTombstone(slotToRow[i])) {
                slotToRow[n++] = slotToRow[i];
            }
        }
        if (n == size) {
            return;
        }
        // Re-link in place: the identity permutation is exactly rows 0..n-1 of the store.
        Arrays.fill(leftChild, NO_CHILD);
        Arrays.fill(rightChild, NO_CHILD);
        buildRoot = buildRange(0, n);
        this.size = n;
        this.tombstones = 0;
    }

    // ---------------------------------------------------------------- build

    /**
     * Recursively builds the subtree over rows {@code [lo,hi)} and returns its root slot.
     *
     * <p>The median row of each range becomes the node and the two halves become its children, so
     * the tree height stays O(log n). Slots are indices into {@link #slotToRow}: the identity
     * permutation leaves row i in slot i.
     */
    private int buildRange(int lo, int hi) {
        int n = hi - lo;
        if (n <= 0) {
            return NO_CHILD;
        }
        if (n == 1) {
            return lo;
        }
        double minX = Double.POSITIVE_INFINITY;
        double maxX = Double.NEGATIVE_INFINITY;
        double minY = Double.POSITIVE_INFINITY;
        double maxY = Double.NEGATIVE_INFINITY;
        for (int i = lo; i < hi; i++) {
            double x = store.xAt(slotToRow[i]);
            double y = store.yAt(slotToRow[i]);
            if (x < minX) {
                minX = x;
            }
            if (x > maxX) {
                maxX = x;
            }
            if (y < minY) {
                minY = y;
            }
            if (y > maxY) {
                maxY = y;
            }
        }
        boolean useX = (maxX - minX) >= (maxY - minY);
        int mid = lo + (n >>> 1);
        selectMedian(slotToRow, lo, hi, mid, useX, store);
        splitAxis[mid] = (byte) (useX ? 0 : 1);
        int left = buildRange(lo, mid);
        int right = buildRange(mid + 1, hi);
        leftChild[mid] = left;
        rightChild[mid] = right;
        return mid;
    }

    private int buildRoot;

    /** Slot id of the tree root, or {@code -1} when the segment holds no points. */
    public int rootSlot() {
        return buildRoot;
    }

    /**
     * Moves the element at {@code pivot} to its final median position using three-way partitioning.
     *
     * <p>Three-way partitioning (rather than Hoare/Lomuto) is essential here: surveying data is full
     * of duplicate coordinates, and a two-way scheme recurses forever when the pivot value repeats.
     * Each call strictly shrinks the range because the equal-valued block is excluded.
     */
    private static void selectMedian(int[] rows, int lo, int hi, int pivot, boolean useX, SegmentStore store) {
        double pivotValue = coord(rows[pivot], useX, store);
        int lt = lo;
        int i = lo;
        int gt = hi - 1;
        while (i <= gt) {
            double c = coord(rows[i], useX, store);
            if (c < pivotValue) {
                int t = rows[lt];
                rows[lt] = rows[i];
                rows[i] = t;
                lt++;
                i++;
            } else if (c > pivotValue) {
                int t = rows[i];
                rows[i] = rows[gt];
                rows[gt] = t;
                gt--;
            } else {
                i++;
            }
        }
        // [lo, lt) < pivotValue, [lt, gt] == pivotValue, (gt, hi) > pivotValue
        if (pivot < lt) {
            selectMedian(rows, lo, lt, pivot, useX, store);
        } else if (pivot > gt) {
            selectMedian(rows, gt + 1, hi, pivot, useX, store);
        }
    }

    private static double coord(int row, boolean useX, SegmentStore store) {
        return useX ? store.xAt(row) : store.yAt(row);
    }

    // ---------------------------------------------------------------- query

    /**
     * k-nearest-neighbour search over live rows.
     *
     * @param probe query point
     * @param k maximum number of neighbours
     * @return up to K neighbours ordered by ascending distance, ties broken by ascending feature id
     */
    public List<Neighbor> knn(GeoPoint probe, int k) {
        if (k <= 0 || size == 0) {
            return List.of();
        }
        PriorityQueue<Neighbor> heap = new PriorityQueue<>(Math.max(1, k), Comparator.reverseOrder());
        double worstSq = Double.POSITIVE_INFINITY;

        int[] stack = new int[Math.min(64, size * 2 + 8)];
        int sp = 0;
        stack[sp++] = buildRoot;

        while (sp > 0) {
            int slot = stack[--sp];
            int row = slotToRow[slot];
            double x = store.xAt(row);
            double y = store.yAt(row);
            double dx = x - probe.x();
            double dy = y - probe.y();
            double dSq = dx * dx + dy * dy;

            if (dSq <= worstSq && !store.isTombstone(row)) {
                Neighbor n = new Neighbor(store.featureAt(row), Math.sqrt(dSq));
                if (heap.size() < k) {
                    heap.add(n);
                } else {
                    Neighbor worst = heap.peek();
                    if (worst.compareTo(n) > 0) {
                        heap.poll();
                        heap.add(n);
                    }
                }
                if (heap.size() == k) {
                    Neighbor worst = heap.peek();
                    worstSq = worst.distance() * worst.distance();
                }
            }

            int l = leftChild[slot];
            int r = rightChild[slot];
            if (l == NO_CHILD && r == NO_CHILD) {
                continue;
            }
            if (sp + 2 > stack.length) {
                stack = Arrays.copyOf(stack, stack.length * 2);
            }
            if (l == NO_CHILD || r == NO_CHILD) {
                // Unbalanced node (an even-sized range leaves one side empty): the only child must
                // always be visited, with no split-plane pruning to do.
                stack[sp++] = (l == NO_CHILD) ? r : l;
                continue;
            }
            // Visit the child on the probe's side first; enqueue the far side only when the split
            // plane falls inside the current K-th best radius.
            boolean useX = splitAxis[slot] == 0;
            double medianValue = useX ? x : y;
            double delta = (useX ? probe.x() : probe.y()) - medianValue;
            int near = delta < 0 ? l : r;
            int far = delta < 0 ? r : l;

            stack[sp++] = near;
            if (delta * delta <= worstSq) {
                stack[sp++] = far;
            }
        }

        List<Neighbor> out = new ArrayList<>(heap);
        out.sort(Neighbor::compareTo);
        return out;
    }

    /** Streams the row index of every live point. */
    public void forEachLiveRow(Consumer<Integer> sink) {
        for (int i = 0; i < size; i++) {
            int row = slotToRow[i];
            if (!store.isTombstone(row)) {
                sink.accept(row);
            }
        }
    }

    /** Visits every slot in tree order (pre-order). */
    public void forEachSlot(IntConsumer sink) {
        for (int i = 0; i < size; i++) {
            sink.accept(slotToRow[i]);
        }
    }

    public int slotCount() {
        return size;
    }

    public int slotRow(int slot) {
        return slotToRow[slot];
    }

    public int slotHeight() {
        return height(0);
    }

    private int height(int slot) {
        if (slot < 0 || slot >= size) {
            return 0;
        }
        int l = leftChild[slot];
        int r = rightChild[slot];
        return 1 + Math.max(l == NO_CHILD ? 0 : height(l), r == NO_CHILD ? 0 : height(r));
    }

    /**
     * Verifies that every slot is reachable from the root exactly once.
     *
     * <p>Used by tests to catch build regressions: an orphaned slot silently drops features from
     * every query result.
     */
    boolean verifyConnected() {
        if (size == 0) {
            return buildRoot == NO_CHILD;
        }
        boolean[] seen = new boolean[size];
        Deque<Integer> stack = new ArrayDeque<>();
        stack.push(buildRoot);
        int visited = 0;
        while (!stack.isEmpty()) {
            int slot = stack.pop();
            if (slot == NO_CHILD || slot >= size) {
                continue;
            }
            if (seen[slot]) {
                return false;
            }
            seen[slot] = true;
            visited++;
            if (leftChild[slot] != NO_CHILD) {
                stack.push(leftChild[slot]);
            }
            if (rightChild[slot] != NO_CHILD) {
                stack.push(rightChild[slot]);
            }
        }
        return visited == size;
    }

    @Override
    public void close() {
        // the segment store is owned by the enclosing segment and closed there
    }
}

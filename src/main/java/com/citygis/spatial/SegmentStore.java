package com.citygis.spatial;

import java.io.IOException;
import java.io.UncheckedIOException;
import java.nio.ByteBuffer;
import java.nio.ByteOrder;
import java.nio.MappedByteBuffer;
import java.nio.channels.FileChannel;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardOpenOption;

/**
 * Off-heap store for one indexed data segment.
 *
 * <p>The segment file has two regions:
 * <ul>
 *   <li>a <b>row region</b> of {@code maxRows} fixed-width 40-byte records starting at the header;</li>
 *   <li>a <b>bulk pool</b> of 8-byte slots after it, holding polygon vertices referenced by
 *       (offset, count) pairs in the owning row.</li>
 * </ul>
 *
 * <p>Rows and bulk data must live in disjoint regions: if a row's vertices were stored immediately
 * after that row, the next row's fixed-width slot would overwrite them. Keeping the pool separate
 * lets both regions be addressed by pure arithmetic, so appends never move existing bytes and the
 * whole segment can be memory-mapped instead of copied onto the heap.
 *
 * <p>Layout (little-endian):
 * <pre>
 *   header: magic int | version short | flags short | count int | poolUsed int | maxRows int
 *   row[i]: id long | type byte | flags byte | pad short | x double | y double
 *           | vertexCount int | vertexOffset int | pad2 int
 *   pool:   vertexCount doubles per row, x/y interleaved
 * </pre>
 */
public final class SegmentStore implements AutoCloseable {

    /** Value written into {@code version} of a freshly created store. */
    public static final short FORMAT_VERSION = 1;

    /**
     * Header field offsets. The magic and version are written once at creation and must never be
     * overwritten by later counter updates, so every offset is named here rather than inlined.
     */
    private static final int OFF_MAGIC = 0;
    private static final int OFF_VERSION = 4;
    private static final int OFF_FLAGS = 6;
    private static final int OFF_COUNT = 8;
    private static final int OFF_POOL_USED = 12;
    private static final int OFF_MAX_ROWS = 16;

    private static final int HEADER_BYTES = 24;

    /**
     * Row field offsets. These must stay in lockstep with {@link #ROW_BYTES}: the two doubles alone
     * need 16 bytes, so the two trailing ints do not fit in a 32-byte row and the row is padded to
     * 40. Every writer and reader goes through these names so the layout cannot drift.
     */
    private static final int ROW_ID = 0;          // long
    private static final int ROW_TYPE = 8;        // byte
    private static final int ROW_FLAGS = 9;       // byte
    private static final int ROW_PAD = 10;        // short
    private static final int ROW_X = 12;          // double
    private static final int ROW_Y = 20;          // double
    private static final int ROW_VERTEX_COUNT = 28;  // int
    private static final int ROW_VERTEX_OFFSET = 32; // int

    private static final int ROW_BYTES = 40;
    private static final int MAGIC = 0x53474D31; // 'SGM1'

    /** Feature is logically deleted; the slot stays until compaction. */
    public static final byte FLAG_TOMBSTONE = 1;

    private final Path path;
    private final MappedByteBuffer mapped;
    private final FileChannel channel;
    private final int maxRows;
    private final int maxPoolSlots;

    private int count;
    private int poolUsed;

    private SegmentStore(Path path, MappedByteBuffer mapped, FileChannel channel,
                         int maxRows, int maxPoolSlots, int count, int poolUsed) {
        this.path = path;
        this.mapped = mapped;
        this.channel = channel;
        this.maxRows = maxRows;
        this.maxPoolSlots = maxPoolSlots;
        this.count = count;
        this.poolUsed = poolUsed;
    }

    /**
     * Creates (or reopens) a store reserving {@code maxRows} row slots and {@code maxPoolSlots}
     * bulk slots.
     */
    public static SegmentStore create(Path path, int maxRows, int maxPoolSlots) {
        if (maxRows <= 0 || maxPoolSlots < 0) {
            throw new IllegalArgumentException("maxRows must be positive and maxPoolSlots non-negative");
        }
        try {
            Path parent = path.toAbsolutePath().getParent();
            if (parent != null) {
                Files.createDirectories(parent);
            }
            long bytes = (long) HEADER_BYTES + (long) maxRows * ROW_BYTES + (long) maxPoolSlots * 8L;
            FileChannel ch = FileChannel.open(path, StandardOpenOption.CREATE,
                    StandardOpenOption.READ, StandardOpenOption.WRITE);
            MappedByteBuffer buf = ch.map(FileChannel.MapMode.READ_WRITE, 0, bytes);
            buf.order(ByteOrder.LITTLE_ENDIAN);
            if (buf.capacity() < bytes) {
                ch.truncate(bytes);
                buf = ch.map(FileChannel.MapMode.READ_WRITE, 0, bytes);
                buf.order(ByteOrder.LITTLE_ENDIAN);
            }
            buf.putInt(OFF_MAGIC, MAGIC);
            buf.putShort(OFF_VERSION, FORMAT_VERSION);
            buf.putShort(OFF_FLAGS, (short) 0);
            buf.putInt(OFF_COUNT, 0);
            buf.putInt(OFF_POOL_USED, 0);
            buf.putInt(OFF_MAX_ROWS, maxRows);
            SegmentStore store = new SegmentStore(path, buf, ch, maxRows, maxPoolSlots, 0, 0);
            store.flushHeader();
            return store;
        } catch (IOException e) {
            throw new UncheckedIOException("cannot create segment store " + path, e);
        }
    }

    /** Opens an existing store for reading. */
    public static SegmentStore openReadOnly(Path path) {
        try (FileChannel ch = FileChannel.open(path, StandardOpenOption.READ)) {
            long size = ch.size();
            if (size < HEADER_BYTES) {
                throw new IllegalStateException("segment store too short: " + path);
            }
            MappedByteBuffer buf = ch.map(FileChannel.MapMode.READ_ONLY, 0, size);
            buf.order(ByteOrder.LITTLE_ENDIAN);
            if (buf.getInt(OFF_MAGIC) != MAGIC) {
                throw new IllegalStateException("bad segment magic in " + path);
            }
            short version = buf.getShort(OFF_VERSION);
            if (version != FORMAT_VERSION) {
                throw new IllegalStateException("unsupported segment version " + version + " in " + path);
            }
            return new SegmentStore(path, buf, ch, buf.getInt(OFF_MAX_ROWS),
                    (int) ((size - HEADER_BYTES) / 8), buf.getInt(OFF_COUNT), buf.getInt(OFF_POOL_USED));
        } catch (IOException e) {
            throw new UncheckedIOException("cannot open segment store " + path, e);
        }
    }

    /*
     * Reopens an existing store for writing.
     *
     * <p>The row capacity recorded in the header is preserved exactly. That matters: the bulk pool
     * starts at {@code HEADER + maxRows * ROW_BYTES}, so widening the row region would shift every
     * pool offset and silently corrupt existing polygons. If a caller genuinely needs more rows than
     * the segment reserved, it must build a new segment instead (see
     * {@link #hasRoomFor(GeoFeature)}).
     *
     * <p>Only the tail of the file may grow here, to give the reopened mapping room for bulk data.
     */
    public static SegmentStore reopen(Path path) {
        try {
            FileChannel ch = FileChannel.open(path, StandardOpenOption.READ, StandardOpenOption.WRITE);
            MappedByteBuffer probe = ch.map(FileChannel.MapMode.READ_ONLY, 0, ch.size());
            probe.order(ByteOrder.LITTLE_ENDIAN);
            if (probe.capacity() < HEADER_BYTES || probe.getInt(OFF_MAGIC) != MAGIC) {
                ch.close();
                throw new IllegalStateException("not a segment store: " + path);
            }
            int maxRows = probe.getInt(OFF_MAX_ROWS);
            int used = probe.getInt(OFF_COUNT);
            int pool = probe.getInt(OFF_POOL_USED);
            long rowRegion = (long) maxRows * ROW_BYTES;
            // Keep maxRows untouched; only ensure the mapping covers the pool plus some headroom.
            // The headroom is bounded by what the rows in use could plausibly need: a store holding
            // no geometry must not reserve hundreds of megabytes of pool just because its row
            // capacity is large.
            long existingPoolBytes = Math.max(0, ch.size() - HEADER_BYTES - rowRegion);
            int headroom = (int) Math.min(Math.max(64L, maxRows / 4L), 1L << 20);
            int poolSlots = (int) Math.max(pool + headroom, existingPoolBytes / 8L);
            long bytes = HEADER_BYTES + rowRegion + (long) poolSlots * 8L;
            if (bytes > Integer.MAX_VALUE) {
                throw new IllegalStateException("segment " + path + " exceeds the 2GB mapping limit");
            }
            if (ch.size() < bytes) {
                ch.truncate(bytes);
            }
            MappedByteBuffer buf = ch.map(FileChannel.MapMode.READ_WRITE, 0, bytes);
            buf.order(ByteOrder.LITTLE_ENDIAN);
            return new SegmentStore(path, buf, ch, maxRows, poolSlots, used, pool);
        } catch (IOException e) {
            throw new UncheckedIOException("cannot reopen segment store " + path, e);
        }
    }

    public int count() {
        return count;
    }

    public int maxRows() {
        return maxRows;
    }

    public int poolUsed() {
        return poolUsed;
    }

    public int poolCapacity() {
        return maxPoolSlots;
    }

    public int remainingRows() {
        return maxRows - count;
    }

    /** Rows needed to store the feature: one row plus its bulk vertices. */
    public int rowsFor(GeoFeature f) {
        return 1;
    }

    /**
     * Bulk slots needed by a feature's vertices.
     *
     * <p>One pool slot is exactly one double, so a vertex array of n doubles costs n slots.
     */
    public int poolSlotsFor(GeoFeature f) {
        return vertexSlots(f);
    }

    /** True when the store can still accept a feature. */
    public boolean hasRoomFor(GeoFeature f) {
        return count < maxRows && poolUsed + poolSlotsFor(f) <= maxPoolSlots;
    }

    /**
     * Number of 8-byte pool slots a feature's vertex array occupies: one slot per double.
     *
     * <p>Getting this wrong (for example by halving it because coordinates come in x/y pairs)
     * makes each polygon overrun the next one's region and silently corrupt the bulk data.
     */
    public static int vertexSlots(GeoFeature f) {
        return f.isArea() ? f.vertices().length : 0;
    }

    public void append(GeoFeature f) {
        append(f, (byte) 0);
    }

    /**
     * Appends a feature with explicit row flags.
     *
     * @param rowFlags {@code 0}, or {@link #FLAG_TOMBSTONE}
     */
    public void append(GeoFeature f, byte rowFlags) {
        if (!hasRoomFor(f)) {
            throw new IllegalStateException("segment store full: rows " + count + "/" + maxRows
                    + ", pool " + poolUsed + "/" + maxPoolSlots);
        }
        int index = count;
        int base = rowOffset(index);
        int vertexCount = f.isArea() ? f.vertices().length : 0;
        int vertexOffset = poolUsed;
        if (vertexCount > 0) {
            poolUsed += vertexCount;
        }
        mapped.putLong(base + ROW_ID, f.id());
        mapped.put(base + ROW_TYPE, (byte) f.type().ordinal());
        mapped.put(base + ROW_FLAGS, rowFlags);
        mapped.putShort(base + ROW_PAD, (short) 0);
        mapped.putDouble(base + ROW_X, f.x());
        mapped.putDouble(base + ROW_Y, f.y());
        mapped.putInt(base + ROW_VERTEX_COUNT, vertexCount);
        mapped.putInt(base + ROW_VERTEX_OFFSET, vertexOffset);
        if (vertexCount > 0) {
            double[] outline = f.vertices();
            int vertexBase = poolByteOffset(vertexOffset);
            for (int i = 0; i < outline.length; i++) {
                mapped.putDouble(vertexBase + i * 8, outline[i]);
            }
        }
        count = index + 1;
    }

    /** Appends a feature and syncs the durable header immediately. */
    public void appendDurable(GeoFeature f) {
        append(f);
        flushHeader();
    }

    public static int headerBytes() {
        return HEADER_BYTES;
    }

    public static int rowBytes() {
        return ROW_BYTES;
    }

    static int rowOffset(int index) {
        return HEADER_BYTES + index * ROW_BYTES;
    }

    /** Reads row {@code index} into a heap feature. */
    public GeoFeature featureAt(int index) {
        int base = rowOffset(index);
        long id = mapped.getLong(base + ROW_ID);
        FeatureType type = FeatureType.values()[mapped.get(base + ROW_TYPE)];
        double x = mapped.getDouble(base + ROW_X);
        double y = mapped.getDouble(base + ROW_Y);
        int vertexCount = mapped.getInt(base + ROW_VERTEX_COUNT);
        if (vertexCount == 0) {
            return GeoFeature.point(id, type, x, y);
        }
        double[] outline = new double[vertexCount];
        int vertexBase = poolByteOffset(mapped.getInt(base + ROW_VERTEX_OFFSET));
        for (int i = 0; i < vertexCount; i++) {
            outline[i] = mapped.getDouble(vertexBase + i * 8);
        }
        return GeoFeature.polygon(id, type, outline);
    }

    private int poolByteOffset(int slotIndex) {
        long offset = (long) HEADER_BYTES + (long) maxRows * ROW_BYTES + (long) slotIndex * 8L;
        if (offset > Integer.MAX_VALUE) {
            throw new IllegalStateException("segment " + path + " exceeds the 2GB addressable window");
        }
        return (int) offset;
    }

    /** Reads the bounding box of row {@code index} without materialising the feature. */
    public BBox bboxAt(int index) {
        int base = rowOffset(index);
        int vertexCount = mapped.getInt(base + ROW_VERTEX_COUNT);
        if (vertexCount == 0) {
            double x = mapped.getDouble(base + ROW_X);
            double y = mapped.getDouble(base + ROW_Y);
            return BBox.ofPoint(x, y);
        }
        int vertexBase = poolByteOffset(mapped.getInt(base + ROW_VERTEX_OFFSET));
        double minX = Double.POSITIVE_INFINITY;
        double minY = Double.POSITIVE_INFINITY;
        double maxX = Double.NEGATIVE_INFINITY;
        double maxY = Double.NEGATIVE_INFINITY;
        for (int i = 0; i < vertexCount; i += 2) {
            double x = mapped.getDouble(vertexBase + i * 8);
            double y = mapped.getDouble(vertexBase + (i + 1) * 8);
            minX = Math.min(minX, x);
            maxX = Math.max(maxX, x);
            minY = Math.min(minY, y);
            maxY = Math.max(maxY, y);
        }
        return new BBox(minX, minY, maxX, maxY);
    }

    /** Reads only the coordinates of row {@code index}. */
    public double xAt(int index) {
        return mapped.getDouble(rowOffset(index) + ROW_X);
    }

    public double yAt(int index) {
        return mapped.getDouble(rowOffset(index) + ROW_Y);
    }

    public long idAt(int index) {
        return mapped.getLong(rowOffset(index) + ROW_ID);
    }

    public FeatureType typeAt(int index) {
        return FeatureType.values()[mapped.get(rowOffset(index) + ROW_TYPE)];
    }

    public byte flagsAt(int index) {
        return mapped.get(rowOffset(index) + ROW_FLAGS);
    }

    public boolean isTombstone(int index) {
        return flagsAt(index) == FLAG_TOMBSTONE;
    }

    /**
     * Marks row {@code index} as a tombstone (lazy delete).
     *
     * @return true when this call flipped a live row to a tombstone
     */
    public boolean tombstone(int index) {
        int base = rowOffset(index);
        if (mapped.get(base + ROW_FLAGS) == FLAG_TOMBSTONE) {
            return false;
        }
        mapped.put(base + ROW_FLAGS, FLAG_TOMBSTONE);
        return true;
    }

    /** Clears the tombstone flag, reviving a row. */
    public boolean revive(int index) {
        int base = rowOffset(index);
        if (mapped.get(base + ROW_FLAGS) != FLAG_TOMBSTONE) {
            return false;
        }
        mapped.put(base + ROW_FLAGS, (byte) 0);
        return true;
    }

    /** Publishes the row count and pool cursor into the durable header. */
    public void flushHeader() {
        mapped.putInt(OFF_COUNT, count);
        mapped.putInt(OFF_POOL_USED, poolUsed);
    }

    /** fsync the segment file. */
    public void sync() {
        try {
            mapped.force();
            channel.force(true);
        } catch (IOException e) {
            throw new UncheckedIOException("cannot sync segment " + path, e);
        }
    }

    /** Total mapped size in bytes. */
    public long mappedBytes() {
        return mapped.capacity();
    }

    public Path path() {
        return path;
    }

    @Override
    public void close() {
        try {
            if (mapped.isLoaded()) {
                mapped.force();
            }
        } catch (RuntimeException ignored) {
            // best effort
        }
        try {
            channel.close();
        } catch (IOException ignored) {
            // best effort
        }
    }
}

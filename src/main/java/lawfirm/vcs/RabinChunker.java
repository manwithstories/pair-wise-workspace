package lawfirm.vcs;

import java.math.BigInteger;
import java.util.ArrayList;
import java.util.List;

/**
 * Content defined chunking driven by a rolling Rabin fingerprint.
 *
 * <p>The fingerprint is the remainder, modulo the irreducible polynomial
 * {@code P = x^128 + x^7 + x^2 + x + 1}, of the bytes in a 16 byte sliding
 * window read as a polynomial over GF(2) with the newest byte as the constant
 * term:
 *
 * <pre>
 *   H(window) = b[0] + b[1]·x^8 + b[2]·x^16 + … + b[15]·x^120   (mod P)
 * </pre>
 *
 * <p>Advancing the window by one byte is a single exact step: multiply by
 * {@code x^8} to shift every weight up one position, add the arriving byte as
 * the new constant term, and subtract the departing byte's contribution at
 * weight {@code x^128}. Because that update is exact, the fingerprint of a
 * window is a function of the 16 bytes it covers alone - it never depends on
 * where the window sits in the file.
 *
 * <p>That property is what makes chunk boundaries content defined. A chunk ends
 * at the first offset at or beyond the minimum size whose fingerprint satisfies
 * the boundary predicate. Editing a few clauses near the front of a 30 MB
 * contract therefore only perturbs the boundaries in the neighbourhood of the
 * edit; every later chunk is segmented at exactly the same offset as before and
 * still hashes to the same id, so the delta encoder can reference it instead of
 * storing it again.
 */
public final class RabinChunker {

    /** Window width in bytes; the fingerprint is 128 bits wide. */
    public static final int WINDOW_SIZE = 16;

    public static final int DEFAULT_AVG_CHUNK_SIZE = 64 * 1024;
    public static final int DEFAULT_MIN_CHUNK_SIZE = 2 * 1024;
    public static final int DEFAULT_MAX_CHUNK_SIZE = 2 * 1024 * 1024;

    private static final BigInteger P = BigInteger.ONE.shiftLeft(128)
            .add(BigInteger.ONE.shiftLeft(7))
            .add(BigInteger.ONE.shiftLeft(2))
            .add(BigInteger.ONE.shiftLeft(1))
            .add(BigInteger.ONE);

    static {
        verifyModulus();
    }

    /** mul[b] = b · x^8 mod P, the reduction term for appending. */
    private final long[] mulTable = new long[256 * 2];
    /** drop[b] = b · x^(8·W) mod P, the departing byte's contribution. */
    private final long[] dropTable = new long[256 * 2];

    private final long maskLo;
    private final long maskHi;
    private final int avgChunk;
    private final int minChunk;
    private final int maxChunk;

    public RabinChunker() {
        this(DEFAULT_AVG_CHUNK_SIZE, DEFAULT_MIN_CHUNK_SIZE, DEFAULT_MAX_CHUNK_SIZE);
    }

    public RabinChunker(int avgChunkSize, int minChunkSize, int maxChunkSize) {
        if (avgChunkSize < WINDOW_SIZE) {
            throw new IllegalArgumentException("avgChunkSize must be >= window size");
        }
        if (minChunkSize < WINDOW_SIZE || minChunkSize > avgChunkSize) {
            throw new IllegalArgumentException("minChunkSize out of range");
        }
        if (maxChunkSize < avgChunkSize) {
            throw new IllegalArgumentException("maxChunkSize must be >= avgChunkSize");
        }
        this.maskLo = maskWord(avgChunkSize, 0);
        this.maskHi = maskWord(avgChunkSize, 1);
        this.avgChunk = avgChunkSize;
        this.minChunk = minChunkSize;
        this.maxChunk = maxChunkSize;
        buildTables();
    }

    /**
     * Builds one word of the boundary mask.
     *
     * <p>The predicate {@code (H[0] & maskLo) == 0 && (H[1] & maskHi) == 0} must
     * hold with probability 1/avgChunkSize, so the mask sets exactly
     * {@code log2(avgChunkSize)} bits in total, spread across the two words of
     * the fingerprint: the low word first, spilling into the high word only once
     * 64 bits are needed.
     */
    static long maskWord(int avgChunkSize, int word) {
        int bits = Integer.numberOfTrailingZeros(Integer.highestOneBit(avgChunkSize));
        if (bits <= 0) return 1L;
        int offset = word * 64;
        int inWord = Math.min(bits, 64) - offset;
        if (inWord <= 0) return 0L;
        if (inWord >= 64) return -1L;
        return (1L << inWord) - 1L;
    }

    /** True when this fingerprint sits on a chunk boundary for these parameters. */
    private boolean isBoundary(long[] h) {
        return (h[0] & maskLo) == 0 && (h[1] & maskHi) == 0;
    }

    public int averageChunkSize() {
        return avgChunk;
    }

    public int minChunkSize() {
        return minChunk;
    }

    public int maxChunkSize() {
        return maxChunk;
    }

    /** One chunk: position in the source, byte length, and SHA-1 of its content. */
    public record Chunk(int offset, int length, ObjectId id) {
    }

    // ------------------------------------------------------- GF(2) helpers

    /** Carry-less multiply of two polynomials, then reduce modulo P. */
    static BigInteger gfMul(BigInteger a, BigInteger b) {
        BigInteger result = BigInteger.ZERO;
        for (int i = 0; i < b.bitLength(); i++) {
            if (b.testBit(i)) {
                result = result.xor(a.shiftLeft(i));
            }
        }
        return gfReduce(result);
    }

    /** Reduce a polynomial of degree below 256 modulo P. */
    static BigInteger gfReduce(BigInteger v) {
        for (int bit = v.bitLength() - 1; bit >= 128; bit--) {
            if (v.testBit(bit)) {
                v = v.xor(P.shiftLeft(bit - 128));
            }
        }
        return v;
    }

    /** x^e mod P by square and multiply; e is a plain non-negative integer. */
    static BigInteger gfPowX(BigInteger e) {
        BigInteger result = BigInteger.ONE;
        BigInteger base = gfReduce(BigInteger.ONE.shiftLeft(1));
        for (int i = 0; i < e.bitLength(); i++) {
            if (e.testBit(i)) {
                result = gfMul(result, base);
            }
            base = gfMul(base, base);
        }
        return result;
    }

    /**
     * P is irreducible over GF(2) exactly when x^(2^128) = x (mod P). A reducible
     * modulus would still yield a deterministic fingerprint, but it would let
     * content changes cancel out and degrade boundary quality, so this is
     * checked once at class initialization rather than trusted.
     */
    private static void verifyModulus() {
        BigInteger x = BigInteger.ONE.shiftLeft(1);
        BigInteger back = gfPowX(BigInteger.ONE.shiftLeft(128));
        if (!back.equals(x)) {
            throw new IllegalStateException("Rabin modulus is not irreducible over GF(2)");
        }
    }

    private static long[] toWords(BigInteger v) {
        return new long[]{v.longValue(), v.shiftRight(64).longValue()};
    }

    private void buildTables() {
        BigInteger x8 = gfPowX(BigInteger.valueOf(8));
        BigInteger xHigh = gfPowX(BigInteger.valueOf(8L * WINDOW_SIZE));
        for (int b = 0; b < 256; b++) {
            BigInteger bv = BigInteger.valueOf(b);
            long[] m = toWords(gfMul(bv, x8));
            long[] d = toWords(gfMul(bv, xHigh));
            mulTable[b * 2] = m[0];
            mulTable[b * 2 + 1] = m[1];
            dropTable[b * 2] = d[0];
            dropTable[b * 2 + 1] = d[1];
        }
    }

    // ------------------------------------------------------------- rolling

    /** Appends one byte: {@code h <- h·x^8 + b} (mod P). */
    private long[] append(long[] h, int b) {
        int top = (int) ((h[1] >>> 56) & 0xffL);
        long lo = (h[0] << 8) ^ mulTable[top * 2] ^ (long) b;
        long hi = (h[1] << 8) | ((h[0] >>> 56) & 0xffL);
        hi ^= mulTable[top * 2 + 1];
        return new long[]{lo, hi};
    }

    /**
     * Exact one byte slide: {@code out} leaves the window and {@code in} enters.
     * The arriving byte first takes the constant term, then the departing byte
     * is removed from the weight it had reached.
     */
    private long[] slide(long[] h, int out, int in) {
        long[] shifted = append(h, in);
        return new long[]{shifted[0] ^ dropTable[out * 2], shifted[1] ^ dropTable[out * 2 + 1]};
    }

    /** The fingerprint of a byte range, computed from scratch. */
    public long[] fingerprintOf(byte[] data, int offset, int length) {
        long[] h = {0L, 0L};
        for (int i = 0; i < length; i++) {
            h = append(h, data[offset + i] & 0xff);
        }
        return h;
    }

    /** The fingerprint a rolling walk would hold for the window ending at {@code end}. */
    public long[] windowFingerprintEndingAt(byte[] data, int start, int end) {
        long[] h = {0L, 0L};
        int from = Math.max(start, end - WINDOW_SIZE);
        for (int i = from; i < end; i++) {
            h = append(h, data[i] & 0xff);
        }
        return h;
    }

    // ------------------------------------------------------------- chunking

    /** Splits the whole array into content defined chunks. */
    public List<Chunk> chunk(byte[] data) {
        return chunk(data, 0, data.length);
    }

    /**
     * Splits {@code data[from, to)} into content defined chunks.
     *
     * <p>A boundary is taken at the first offset at or beyond {@code minChunk}
     * where both mask words of the fingerprint are zero, and forced at
     * {@code maxChunk} so that adversarial or incompressible input still yields
     * bounded chunks. Any tail shorter than the window is emitted as a final
     * chunk.
     */
    public List<Chunk> chunk(byte[] data, int from, int to) {
        List<Chunk> out = new ArrayList<>();
        if (to <= from) {
            out.add(new Chunk(from, 0, ObjectId.hash(new byte[0])));
            return out;
        }
        int start = from;
        int pos = from;
        long[] h = {0L, 0L};
        int length = 0;
        // Prime the window before looking for boundaries; a partial window would
        // otherwise decide the first boundary from padding rather than content.
        while (length < WINDOW_SIZE && pos < to) {
            h = append(h, data[pos++] & 0xff);
            length++;
        }
        while (true) {
            if (pos >= to) break;
            boolean boundary = length >= minChunk && isBoundary(h);
            if (!boundary && length >= maxChunk) {
                boundary = true;
            }
            if (boundary) {
                out.add(makeChunk(data, start, length));
                start += length;
                pos = start;
                length = 0;
                h = new long[]{0L, 0L};
                while (length < WINDOW_SIZE && pos < to) {
                    h = append(h, data[pos++] & 0xff);
                    length++;
                }
            } else {
                h = slide(h, data[pos - WINDOW_SIZE] & 0xff, data[pos] & 0xff);
                pos++;
                length++;
            }
        }
        if (start < to) {
            out.add(makeChunk(data, start, to - start));
        }
        return out;
    }

    private Chunk makeChunk(byte[] data, int offset, int length) {
        java.security.MessageDigest md = ObjectId.digest();
        md.update(data, offset, length);
        return new Chunk(offset, length, ObjectId.of(md.digest()));
    }
}

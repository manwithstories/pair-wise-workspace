package lawfirm.vcs;

import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.Arrays;

/**
 * A git object identifier: the raw 20 byte SHA-1 digest of a canonical object
 * representation, plus hex encoding/decoding and unsigned global ordering.
 *
 * <p>Every comparison in this VCS is a byte-wise unsigned comparison, which is
 * exactly what git uses when it orders objects for indexing or display. The
 * class is immutable; {@link #bytes()} hands out a defensive copy.
 */
public final class ObjectId implements Comparable<ObjectId> {

    public static final int LENGTH = 20;
    public static final int HEX_LENGTH = 2 * LENGTH;

    private static final char[] HEX = "0123456789abcdef".toCharArray();
    private static final ObjectId ZERO = new ObjectId(new byte[LENGTH]);

    private final byte[] bytes;
    private String hex; // lazily computed, benign race: the value is deterministic

    private ObjectId(byte[] bytes) {
        if (bytes.length != LENGTH) {
            throw new IllegalArgumentException("object id must be " + LENGTH + " bytes, got " + bytes.length);
        }
        this.bytes = bytes;
    }

    public static ObjectId of(byte[] raw) {
        return new ObjectId(raw.clone());
    }

    public static ObjectId of(byte[] raw, int offset) {
        return new ObjectId(Arrays.copyOfRange(raw, offset, offset + LENGTH));
    }

    public static ObjectId zero() {
        return ZERO;
    }

    /** Parses a 40 character hex string (either case). */
    public static ObjectId fromHex(String hex) {
        return new ObjectId(decodeHex(hex));
    }

    private static byte[] decodeHex(String hex) {
        if (hex == null || hex.length() != HEX_LENGTH) {
            throw new IllegalArgumentException("not a 40 character hex object id: " + hex);
        }
        byte[] out = new byte[LENGTH];
        for (int i = 0; i < LENGTH; i++) {
            int hi = digit(hex.charAt(i * 2));
            int lo = digit(hex.charAt(i * 2 + 1));
            out[i] = (byte) ((hi << 4) | lo);
        }
        return out;
    }

    private static int digit(char c) {
        if (c >= '0' && c <= '9') return c - '0';
        if (c >= 'a' && c <= 'f') return c - 'a' + 10;
        if (c >= 'A' && c <= 'F') return c - 'A' + 10;
        throw new IllegalArgumentException("invalid hex character: " + c);
    }

    public static MessageDigest newSha1() {
        try {
            return MessageDigest.getInstance("SHA-1");
        } catch (NoSuchAlgorithmException e) {
            throw new IllegalStateException("SHA-1 is required by the git object model", e);
        }
    }

    /** SHA-1 of an already assembled canonical object payload. */
    public static ObjectId hash(byte[] canonical) {
        return new ObjectId(newSha1().digest(canonical));
    }

    /** Incremental hashing helper for payloads that are produced by a stream. */
    public static MessageDigest digest() {
        return newSha1();
    }

    public byte[] bytes() {
        return bytes.clone();
    }

    /** Direct array access; callers must not mutate the result. */
    public byte[] raw() {
        return bytes;
    }

    public String hex() {
        String h = hex;
        if (h == null) {
            char[] out = new char[HEX_LENGTH];
            for (int i = 0; i < LENGTH; i++) {
                int v = bytes[i] & 0xff;
                out[i * 2] = HEX[v >>> 4];
                out[i * 2 + 1] = HEX[v & 0x0f];
            }
            h = new String(out);
            hex = h;
        }
        return h;
    }

    @Override
    public String toString() {
        return hex();
    }

    @Override
    public boolean equals(Object o) {
        return o instanceof ObjectId other && Arrays.equals(bytes, other.bytes);
    }

    @Override
    public int hashCode() {
        return Arrays.hashCode(bytes);
    }

    /** Unsigned byte-wise comparison; the canonical "global order" of git objects. */
    @Override
    public int compareTo(ObjectId o) {
        for (int i = 0; i < LENGTH; i++) {
            int d = (bytes[i] & 0xff) - (o.bytes[i] & 0xff);
            if (d != 0) return d;
        }
        return 0;
    }

    /** First differing byte index, or -1 when equal; handy for radix ordering. */
    public int firstDiff(ObjectId o) {
        for (int i = 0; i < LENGTH; i++) {
            if (bytes[i] != o.bytes[i]) return i;
        }
        return -1;
    }

    /** Short prefix, for human readable dumps only. Never used for addressing. */
    public String abbrev(int n) {
        return hex().substring(0, Math.min(n, HEX_LENGTH));
    }

    public static int compare(byte[] a, byte[] b) {
        int n = Math.min(a.length, b.length);
        for (int i = 0; i < n; i++) {
            int d = (a[i] & 0xff) - (b[i] & 0xff);
            if (d != 0) return d;
        }
        return a.length - b.length;
    }
}

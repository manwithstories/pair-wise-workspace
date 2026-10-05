package lawfirm.vcs;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import java.nio.charset.StandardCharsets;
import java.util.Arrays;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

class ObjectIdTest {

    @Test
    @DisplayName("SHA-1 of the empty blob matches the well known git id")
    void emptyBlobIdMatchesGit() {
        // git hash-object of an empty file
        assertEquals("e69de29bb2d1d6434b8b29ae775ad8c2e48c5391",
                GitObject.blob(new byte[0]).id().hex());
    }

    @Test
    @DisplayName("SHA-1 of 'hello\\n' matches the well known git id")
    void helloBlobIdMatchesGit() {
        assertEquals("ce013625030ba8dba906f756967f9e9ca394464a",
                GitObject.blob("hello\n".getBytes(StandardCharsets.UTF_8)).id().hex());
    }

    @Test
    @DisplayName("hex round trips in both cases")
    void hexRoundTrip() {
        byte[] raw = new byte[20];
        for (int i = 0; i < 20; i++) raw[i] = (byte) (i * 7 + 3);
        ObjectId id = ObjectId.of(raw);
        assertEquals(id, ObjectId.fromHex(id.hex()));
        assertEquals(id, ObjectId.fromHex(id.hex().toUpperCase()));
        assertEquals(40, id.hex().length());
    }

    @Test
    @DisplayName("fromHex rejects malformed input")
    void fromHexRejectsGarbage() {
        assertThrows(IllegalArgumentException.class, () -> ObjectId.fromHex("abc"));
        assertThrows(IllegalArgumentException.class, () -> ObjectId.fromHex("zz" + "0".repeat(38)));
        assertThrows(IllegalArgumentException.class, () -> ObjectId.fromHex(null));
    }

    @Test
    @DisplayName("comparison is unsigned byte-wise, not signed")
    void comparisonIsUnsigned() {
        byte[] low = new byte[20];   // all zero
        byte[] high = new byte[20];
        high[19] = (byte) 0xff;      // last byte 0xff, would be negative if signed
        ObjectId a = ObjectId.of(low);
        ObjectId b = ObjectId.of(high);
        assertTrue(a.compareTo(b) < 0, "0x00.. must sort before 0x..ff unsigned");
        assertTrue(b.compareTo(a) > 0);
        assertEquals(0, a.compareTo(ObjectId.of(low.clone())));
    }

    @Test
    @DisplayName("firstDiff locates the first differing byte")
    void firstDiffFindsDivergence() {
        byte[] a = new byte[20];
        byte[] b = new byte[20];
        b[7] = 1;
        assertEquals(7, ObjectId.of(a).firstDiff(ObjectId.of(b)));
        assertEquals(-1, ObjectId.of(a).firstDiff(ObjectId.of(a.clone())));
    }

    @Test
    @DisplayName("bytes() hands out a defensive copy")
    void bytesAreDefensiveCopies() {
        ObjectId id = ObjectId.fromHex("0000000000000000000000000000000000000001");
        byte[] first = id.bytes();
        first[0] = 0x7f;
        assertArrayEquals(id.bytes(), id.raw(), "mutating the copy must not affect the id");
    }

    @Test
    @DisplayName("of(raw, offset) reads 20 bytes from an array")
    void offsetConstruction() {
        byte[] buffer = new byte[24];
        buffer[4] = 1;               // window is buffer[4..24), so this is byte 0
        ObjectId id = ObjectId.of(buffer, 4);
        assertEquals("0100000000000000000000000000000000000000", id.hex());
    }

    @Test
    @DisplayName("equality and hashing agree")
    void equalityAndHashing() {
        String hex = "0123456789abcdef0123456789abcdef01234567";
        ObjectId a = ObjectId.fromHex(hex);
        ObjectId b = ObjectId.fromHex(hex);
        assertEquals(a, b);
        assertEquals(a.hashCode(), b.hashCode());
        assertNotEquals(a, ObjectId.zero());
    }

    @Test
    @DisplayName("sorting a list yields the canonical global order")
    void sortingUsesGlobalOrder() {
        ObjectId a = ObjectId.fromHex("ff00000000000000000000000000000000000000");
        ObjectId b = ObjectId.fromHex("00000000000000000000000000000000000000ff");
        ObjectId[] arr = {a, b};
        Arrays.sort(arr);
        assertEquals(b, arr[0]);
        assertEquals(a, arr[1]);
    }
}

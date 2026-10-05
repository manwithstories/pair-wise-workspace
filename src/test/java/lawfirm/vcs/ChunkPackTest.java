package lawfirm.vcs;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Path;
import java.util.Arrays;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

class ChunkPackTest {

    /** Small chunk parameters keep the tests fast while exercising every path. */
    private static ChunkPack pack(Path dir) {
        return new ChunkPack(dir, new RabinChunker(4096, 256, 32768));
    }

    @Test
    @DisplayName("a blob round trips byte for byte")
    void roundTrip(@TempDir Path dir) throws IOException {
        ChunkPack pack = pack(dir);
        byte[] content = Contracts.text(2000, 1);
        ChunkPack.StoredBlob stored = pack.store(content, null);
        assertArrayEquals(content, pack.readBlob(stored.id()));
    }

    @Test
    @DisplayName("the logical id is the git blob id, unchanged by chunking")
    void logicalIdIsGitBlobId(@TempDir Path dir) throws IOException {
        ChunkPack pack = pack(dir);
        byte[] content = Contracts.text(2000, 2);
        ChunkPack.StoredBlob stored = pack.store(content, null);
        assertEquals(GitObject.blob(content).id(), stored.id(),
                "the address must be what native git would compute");
    }

    @Test
    @DisplayName("a delta version keeps the same logical id convention")
    void deltaKeepsLogicalId(@TempDir Path dir) throws IOException {
        ChunkPack pack = pack(dir);
        byte[] v1 = Contracts.text(2000, 3);
        byte[] v2 = Contracts.amend(v1, 500);
        ChunkPack.StoredBlob first = pack.store(v1, null);
        ChunkPack.StoredBlob second = pack.store(v2, first.id());

        assertEquals(GitObject.blob(v2).id(), second.id());
        assertArrayEquals(v2, pack.readBlob(second.id()));
        assertNotEquals(first.id(), second.id());
    }

    @Test
    @DisplayName("a second version costs far less than storing it whole")
    void secondVersionIsSmall(@TempDir Path dir) throws IOException {
        ChunkPack pack = pack(dir);
        byte[] v1 = Contracts.text(8000, 4);
        long afterFirst = storeAndMeasure(pack, v1, null);

        byte[] v2 = Contracts.amend(v1, 999);
        long delta = storeAndMeasure(pack, v2, GitObject.blob(v1).id());

        double ratio = delta / (double) v2.length;
        assertTrue(ratio < 0.05,
                "delta should be under 5% of the full file: " + delta + " vs " + v2.length);
        assertTrue(afterFirst > 0);
    }

    private static long storeAndMeasure(ChunkPack pack, byte[] content, ObjectId base)
            throws IOException {
        long before = pack.physicalBytes();
        pack.store(content, base);
        return pack.physicalBytes() - before;
    }

    @Test
    @DisplayName("twenty successive amendments stay far below the full size")
    void manyVersionsStaySmall(@TempDir Path dir) throws IOException {
        ChunkPack pack = pack(dir);
        byte[] current = Contracts.text(8000, 5);
        pack.store(current, null);
        long fullSize = current.length;
        long baseline = pack.physicalBytes();

        for (int v = 0; v < 20; v++) {
            byte[] next = Contracts.amend(current, 2000 + v);
            ChunkPack.StoredBlob stored = pack.store(next, GitObject.blob(current).id());
            assertArrayEquals(next, pack.readBlob(stored.id()), "version " + v + " round trip");
            current = next;
        }
        long growth = pack.physicalBytes() - baseline;
        double ratio = growth / fullSize;
        assertTrue(ratio < 0.05,
                "20 amendments grew the store by " + growth + " bytes, " + (ratio * 100)
                        + "% of one full version");
    }

    @Test
    @DisplayName("delta versions reuse chunks rather than duplicating them")
    void deltaReusesChunks(@TempDir Path dir) throws IOException {
        ChunkPack pack = pack(dir);
        byte[] v1 = Contracts.text(4000, 6);
        pack.store(v1, null);
        int chunksAfterFirst = pack.chunkFileCount();

        byte[] v2 = Contracts.amend(v1, 1);
        pack.store(v2, GitObject.blob(v1).id());
        int chunksAfterSecond = pack.chunkFileCount();

        int added = chunksAfterSecond - chunksAfterFirst;
        int total = pack.chunkListOf(GitObject.blob(v2).id()).size();
        assertTrue(added < total / 4,
                "only the edited neighbourhood should be new: " + added + " of " + total);
    }

    @Test
    @DisplayName("a small blob is stored verbatim")
    void smallBlobsAreVerbatim(@TempDir Path dir) throws IOException {
        ChunkPack pack = pack(dir);
        byte[] content = "tiny contract clause".getBytes(StandardCharsets.UTF_8);
        ChunkPack.StoredBlob stored = pack.store(content, null);
        assertArrayEquals(content, pack.readBlob(stored.id()));
        assertEquals(null, stored.base());
    }

    @Test
    @DisplayName("storing identical content twice is idempotent")
    void storingTwiceIsIdempotent(@TempDir Path dir) throws IOException {
        ChunkPack pack = pack(dir);
        byte[] content = Contracts.text(1000, 7);
        ChunkPack.StoredBlob a = pack.store(content, null);
        long sizeAfterFirst = pack.physicalBytes();
        ChunkPack.StoredBlob b = pack.store(content, null);
        assertEquals(a.id(), b.id());
        assertEquals(sizeAfterFirst, pack.physicalBytes(), "no new bytes for identical content");
    }

    @Test
    @DisplayName("unrelated content falls back to storing its own chunks")
    void unrelatedContentIsNotDeltaEncoded(@TempDir Path dir) throws IOException {
        ChunkPack pack = pack(dir);
        byte[] v1 = Contracts.text(3000, 8);
        pack.store(v1, null);

        byte[] noise = new byte[v1.length];
        new java.util.Random(99).nextBytes(noise);
        ChunkPack.StoredBlob stored = pack.store(noise, GitObject.blob(v1).id());
        assertArrayEquals(noise, pack.readBlob(stored.id()));
    }

    @Test
    @DisplayName("a chain of deltas reconstructs every version exactly")
    void deltaChainReconstructs(@TempDir Path dir) throws IOException {
        ChunkPack pack = pack(dir);
        byte[] v1 = Contracts.text(3000, 9);
        ObjectId prev = pack.store(v1, null).id();

        byte[] current = v1;
        for (int i = 0; i < 5; i++) {
            byte[] next = Contracts.amend(current, 100 + i);
            ObjectId id = pack.store(next, prev).id();
            assertArrayEquals(next, pack.readBlob(id), "version " + i);
            prev = id;
            current = next;
        }
    }

    @Test
    @DisplayName("reading an unknown blob fails loudly")
    void unknownBlobFails(@TempDir Path dir) {
        ChunkPack pack = pack(dir);
        assertThrows(IOException.class, () -> pack.readBlob(ObjectId.zero()));
    }

    @Test
    @DisplayName("knownBlobs lists every stored version in ascending order")
    void knownBlobsAreSorted(@TempDir Path dir) throws IOException {
        ChunkPack pack = pack(dir);
        byte[] v1 = Contracts.text(500, 10);
        byte[] v2 = Contracts.amend(v1, 1);
        pack.store(v1, null);
        pack.store(v2, GitObject.blob(v1).id());

        var ids = pack.knownBlobs();
        assertEquals(2, ids.size());
        assertTrue(ids.get(0).compareTo(ids.get(1)) < 0, "ids must be ascending");
        assertTrue(ids.contains(GitObject.blob(v1).id()));
        assertTrue(ids.contains(GitObject.blob(v2).id()));
    }

    @Test
    @DisplayName("the chunk list of a delta version resolves through its base")
    void chunkListResolvesThroughBase(@TempDir Path dir) throws IOException {
        ChunkPack pack = pack(dir);
        byte[] v1 = Contracts.text(2000, 11);
        pack.store(v1, null);
        byte[] v2 = Contracts.amend(v1, 3);
        pack.store(v2, GitObject.blob(v1).id());

        var list = pack.chunkListOf(GitObject.blob(v2).id());
        assertFalse(list.isEmpty());
        long total = 0;
        for (ObjectId cid : list) total += pack.readChunkForTest(cid).length;
        assertEquals(v2.length, total, "chunks must reassemble into the exact content");
    }

    @Test
    @DisplayName("physical size never exceeds the deflated full size for a delta")
    void deltaBeatsStoringTheWholeFile(@TempDir Path dir) throws IOException {
        ChunkPack pack = pack(dir);
        byte[] v1 = Contracts.text(6000, 12);
        pack.store(v1, null);
        long baseline = pack.physicalBytes();

        byte[] v2 = Contracts.amend(v1, 4);
        long growth = storeAndMeasure(pack, v2, GitObject.blob(v1).id());

        long deflatedWhole = deflatedLength(v2);
        assertTrue(growth < deflatedWhole,
                "delta (" + growth + ") must be smaller than a whole deflated copy ("
                        + deflatedWhole + ")");
        assertTrue(baseline > 0);
    }

    private static long deflatedLength(byte[] data) {
        var d = new java.util.zip.Deflater(java.util.zip.Deflater.DEFAULT_COMPRESSION);
        try {
            d.setInput(data);
            d.finish();
            long total = 0;
            byte[] buf = new byte[8192];
            while (!d.finished()) total += d.deflate(buf);
            return total;
        } finally {
            d.end();
        }
    }

    @Test
    @DisplayName("binary content survives the round trip unchanged")
    void binaryContentRoundTrips(@TempDir Path dir) throws IOException {
        ChunkPack pack = pack(dir);
        byte[] v1 = new byte[3_000_000];
        new java.util.Random(13).nextBytes(v1);
        pack.store(v1, null);

        byte[] v2 = Arrays.copyOf(v1, v1.length + 64);
        new java.util.Random(14).nextBytes(Arrays.copyOfRange(v2, v1.length, v2.length));
        ChunkPack.StoredBlob stored = pack.store(v2, GitObject.blob(v1).id());

        assertArrayEquals(v2, pack.readBlob(stored.id()));
    }
}

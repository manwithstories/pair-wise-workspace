package lawfirm.vcs;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.HashSet;
import java.util.List;
import java.util.Map;
import java.util.Random;
import java.util.Set;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

class RabinChunkerTest {

    /** Small parameters so tests exercise many boundaries quickly. */
    private static RabinChunker small() {
        return new RabinChunker(1024, 64, 4096);
    }

    private static byte[] randomBytes(long seed, int length) {
        byte[] data = new byte[length];
        new Random(seed).nextBytes(data);
        return data;
    }

    private static Set<ObjectId> ids(List<RabinChunker.Chunk> chunks) {
        Set<ObjectId> out = new HashSet<>();
        for (RabinChunker.Chunk c : chunks) out.add(c.id());
        return out;
    }

    @Test
    @DisplayName("the rolling fingerprint equals a from-scratch fingerprint")
    void rollingUpdateIsExact() {
        RabinChunker rc = small();
        byte[] data = randomBytes(1, 50_000);
        for (int end = 16; end < 2000; end += 7) {
            long[] rolled = rc.windowFingerprintEndingAt(data, 0, end);
            long[] scratch = rc.fingerprintOf(data, end - 16, 16);
            assertEquals(java.util.Arrays.toString(scratch), java.util.Arrays.toString(rolled),
                    "window ending at " + end + " must hash identically either way");
        }
    }

    @Test
    @DisplayName("the fingerprint depends only on the window's bytes, not its position")
    void fingerprintIsPositionIndependent() {
        RabinChunker rc = small();
        byte[] window = "the quick brown fox!".getBytes(StandardCharsets.UTF_8);
        byte[] embedded = new byte[window.length * 4];
        System.arraycopy(window, 0, embedded, window.length, window.length);
        assertEquals(java.util.Arrays.toString(rc.fingerprintOf(window, 0, window.length)),
                java.util.Arrays.toString(rc.fingerprintOf(embedded, window.length, window.length)));
    }

    @Test
    @DisplayName("chunks tile the input exactly, in order, with no gaps or overlaps")
    void chunksCoverInputExactly() {
        RabinChunker rc = small();
        byte[] data = randomBytes(2, 200_000);
        List<RabinChunker.Chunk> chunks = rc.chunk(data);

        int cursor = 0;
        for (RabinChunker.Chunk c : chunks) {
            assertEquals(cursor, c.offset(), "chunks must be contiguous");
            assertTrue(c.length() > 0, "no empty chunks");
            cursor += c.length();
        }
        assertEquals(data.length, cursor, "chunks must cover the whole input");
    }

    @Test
    @DisplayName("chunk ids are the SHA-1 of their content")
    void chunkIdIsSha1OfContent() {
        RabinChunker rc = small();
        byte[] data = randomBytes(3, 20_000);
        for (RabinChunker.Chunk c : rc.chunk(data)) {
            assertEquals(ObjectId.hash(java.util.Arrays.copyOfRange(data, c.offset(),
                    c.offset() + c.length())), c.id());
        }
    }

    @Test
    @DisplayName("chunking is deterministic")
    void chunkingIsDeterministic() {
        RabinChunker rc = small();
        byte[] data = randomBytes(4, 50_000);
        assertEquals(rc.chunk(data), rc.chunk(data));
    }

    @Test
    @DisplayName("the average chunk size tracks the configured target")
    void averageChunkSizeIsNearTarget() {
        RabinChunker rc = new RabinChunker(8192, 512, 65536);
        byte[] data = randomBytes(5, 4_000_000);
        List<RabinChunker.Chunk> chunks = rc.chunk(data);
        double avg = data.length / (double) chunks.size();
        assertTrue(avg > 8192 * 0.7 && avg < 8192 * 1.4,
                "average " + (long) avg + " should be near the 8192 target");
    }

    @Test
    @DisplayName("chunk sizes respect the configured minimum and maximum")
    void chunkSizesAreBounded() {
        RabinChunker rc = new RabinChunker(8192, 1024, 65536);
        byte[] data = randomBytes(6, 4_000_000);
        List<RabinChunker.Chunk> chunks = rc.chunk(data);
        for (RabinChunker.Chunk c : chunks) {
            // only the final chunk may fall below the minimum
            boolean last = c.offset() + c.length() == data.length;
            assertTrue(c.length() <= 65536, "chunk exceeded max: " + c.length());
            assertTrue(last || c.length() >= 1024 - RabinChunker.WINDOW_SIZE,
                    "interior chunk below min: " + c.length());
        }
    }

    @Test
    @DisplayName("pathological input of one repeated byte still yields bounded chunks")
    void degenerateInputIsBounded() {
        RabinChunker rc = new RabinChunker(4096, 256, 16384);
        byte[] data = new byte[200_000];
        java.util.Arrays.fill(data, (byte) 'A');
        List<RabinChunker.Chunk> chunks = rc.chunk(data);
        assertTrue(chunks.size() > 1);
        assertEquals(data.length, chunks.stream().mapToLong(RabinChunker.Chunk::length).sum());
    }

    @Test
    @DisplayName("an insertion perturbs only the chunks around it")
    void insertionPreservesMostChunks() {
        RabinChunker rc = small();
        byte[] original = randomBytes(7, 300_000);
        byte[] edited = splice(original, 1000, "INSERTED".getBytes(StandardCharsets.UTF_8));

        Set<ObjectId> before = ids(rc.chunk(original));
        List<RabinChunker.Chunk> after = rc.chunk(edited);
        long shared = after.stream().filter(c -> before.contains(c.id())).count();

        double ratio = shared / (double) after.size();
        assertTrue(ratio > 0.9, "expected >90% chunk reuse, got " + (ratio * 100) + "%");
    }

    @Test
    @DisplayName("a deletion also preserves most chunks")
    void deletionPreservesMostChunks() {
        RabinChunker rc = small();
        byte[] original = randomBytes(8, 300_000);
        byte[] edited = new byte[original.length - 1000];
        System.arraycopy(original, 0, edited, 0, 500);
        System.arraycopy(original, 1500, edited, 500, original.length - 1500);

        Set<ObjectId> before = ids(rc.chunk(original));
        List<RabinChunker.Chunk> after = rc.chunk(edited);
        long shared = after.stream().filter(c -> before.contains(c.id())).count();
        double ratio = shared / (double) after.size();
        assertTrue(ratio > 0.9, "expected >90% chunk reuse, got " + (ratio * 100) + "%");
    }

    @Test
    @DisplayName("contract-like text reuses chunks when a clause is inserted")
    void realisticContractTextReusesChunks() {
        RabinChunker rc = new RabinChunker(8192, 1024, 65536);
        byte[] contract = Contracts.text(4000, 12345);
        byte[] amended = splice(contract, contract.length / 3,
                "第二十一条　【保密义务】　乙方不得向任何第三方披露本合同内容。\n"
                        .getBytes(StandardCharsets.UTF_8));

        Set<ObjectId> before = ids(rc.chunk(contract));
        List<RabinChunker.Chunk> after = rc.chunk(amended);
        long shared = after.stream().filter(c -> before.contains(c.id())).count();
        double ratio = shared / (double) after.size();
        assertTrue(ratio > 0.8, "expected >80% reuse on contract text, got " + (ratio * 100) + "%");
    }

    @Test
    @DisplayName("identical content at different offsets chunks identically")
    void repeatedContentChunksConsistently() {
        RabinChunker rc = small();
        byte[] block = randomBytes(9, 30_000);
        byte[] doubled = new byte[block.length * 3];
        for (int i = 0; i < 3; i++) System.arraycopy(block, 0, doubled, i * block.length, block.length);

        Map<ObjectId, Integer> counts = new HashMap<>();
        for (RabinChunker.Chunk c : rc.chunk(doubled)) {
            counts.merge(c.id(), 1, Integer::sum);
        }
        long maxRepeat = counts.values().stream().mapToLong(Integer::longValue).max().orElse(0);
        assertTrue(maxRepeat >= 3,
                "three identical copies should produce repeated chunk ids, saw " + counts.size()
                        + " distinct ids");
    }

    @Test
    @DisplayName("the mask sets exactly log2(avg) bits across the two words")
    void maskHasTheRightPopulation() {
        for (int avg : new int[]{1024, 4096, 65536}) {
            long bits = Long.bitCount(RabinChunker.maskWord(avg, 0))
                    + Long.bitCount(RabinChunker.maskWord(avg, 1));
            assertEquals(Integer.numberOfTrailingZeros(Integer.highestOneBit(avg)), bits,
                    "mask population for avg=" + avg);
        }
    }

    @Test
    @DisplayName("chunk-stats style summary agrees with the chunk list")
    void statsMatchChunkList() {
        RabinChunker rc = small();
        byte[] data = randomBytes(10, 100_000);
        List<RabinChunker.Chunk> chunks = rc.chunk(data);
        long max = chunks.stream().mapToLong(RabinChunker.Chunk::length).max().orElse(0);
        long min = chunks.stream().mapToLong(RabinChunker.Chunk::length).min().orElse(0);
        assertTrue(min > 0 && max >= min);
        assertEquals(data.length, chunks.stream().mapToLong(RabinChunker.Chunk::length).sum());
    }

    @Test
    @DisplayName("invalid chunker parameters are rejected")
    void invalidParametersRejected() {
        assertThrows(IllegalArgumentException.class,
                () -> new RabinChunker(8, 4, 64));
        assertThrows(IllegalArgumentException.class,
                () -> new RabinChunker(1024, 2048, 4096));
        assertThrows(IllegalArgumentException.class,
                () -> new RabinChunker(4096, 64, 1024));
    }

    @Test
    @DisplayName("empty input yields a single empty chunk")
    void emptyInput() {
        RabinChunker rc = small();
        List<RabinChunker.Chunk> chunks = rc.chunk(new byte[0]);
        assertEquals(1, chunks.size());
        assertEquals(0, chunks.get(0).length());
    }

    @Test
    @DisplayName("input shorter than the window is one chunk")
    void tinyInput() {
        RabinChunker rc = small();
        byte[] data = randomBytes(11, 10);
        List<RabinChunker.Chunk> chunks = rc.chunk(data);
        assertEquals(1, chunks.size());
        assertEquals(10, chunks.get(0).length());
    }

    private static byte[] splice(byte[] original, int at, byte[] insert) {
        byte[] out = new byte[original.length + insert.length];
        System.arraycopy(original, 0, out, 0, at);
        System.arraycopy(insert, 0, out, at, insert.length);
        System.arraycopy(original, at, out, at + insert.length, original.length - at);
        return out;
    }
}

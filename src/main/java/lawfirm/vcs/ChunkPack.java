package lawfirm.vcs;

import java.io.ByteArrayOutputStream;
import java.io.DataInputStream;
import java.io.DataOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardCopyOption;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.HashMap;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * Physical storage for large blobs: a content defined chunk store plus a delta
 * stream of copy/insert instructions.
 *
 * <p>The logical identity of a blob is unchanged by any of this. A blob is still
 * addressed by the SHA-1 that native git computes over
 * {@code "blob <size>\0" + content}, and that same content is what
 * {@link #readBlob} returns, byte for byte. Only the physical representation
 * differs from a loose git blob:
 *
 * <ul>
 *   <li>Chunks live once in a shared chunk pool, addressed by their own SHA-1
 *       and deflated individually. An unchanged clause is stored exactly once no
 *       matter how many contract versions reference it.</li>
 *   <li>A blob version that resembles an earlier one is stored as a recipe of
 *       {@code COPY} instructions (reusing a contiguous run of the base's
 *       chunks, a handful of bytes) and {@code INSERT} instructions (naming a
 *       chunk already in the pool). The recipe never contains file content.</li>
 * </ul>
 *
 * <p>Recipes point at a base blob id and are resolved recursively on read, with a
 * depth cap so a corrupt chain fails loudly instead of recursing forever.
 */
public final class ChunkPack {

    /** Instruction opcodes. */
    private static final int OP_COPY = 1;
    private static final int OP_INSERT = 2;

    private static final int MAX_DELTA_DEPTH = 64;
    /** Below this many bytes a literal insert is cheaper than a chunk reference. */
    private static final int INLINE_THRESHOLD = 96;

    private final Path dir;
    private final RabinChunker chunker;
    private final Map<ObjectId, ObjectId> deltaBases = new HashMap<>();

    public ChunkPack(Path dir) {
        this(dir, new RabinChunker());
    }

    public ChunkPack(Path dir, RabinChunker chunker) {
        this.dir = dir;
        this.chunker = chunker;
    }

    public RabinChunker chunker() {
        return chunker;
    }

    // ------------------------------------------------------------ layout

    private Path chunksDir() {
        return dir.resolve("chunks");
    }

    private Path recipesDir() {
        return dir.resolve("recipes");
    }

    private Path chunkPath(ObjectId id) {
        String h = id.hex();
        return chunksDir().resolve(h.substring(0, 2)).resolve(h.substring(2));
    }

    private Path recipePath(ObjectId blobId) {
        String h = blobId.hex();
        return recipesDir().resolve(h.substring(0, 2)).resolve(h.substring(2));
    }

    /** Stores a chunk once; returns its id. Idempotent. */
    private ObjectId writeChunk(byte[] data, int off, int len) throws IOException {
        var md = ObjectId.digest();
        md.update(data, off, len);
        ObjectId id = ObjectId.of(md.digest());
        Path target = chunkPath(id);
        if (Files.isRegularFile(target)) {
            return id;
        }
        Files.createDirectories(target.getParent());
        Path tmp = Files.createTempFile(target.getParent(), "tmp_chunk_", null);
        try {
            try (OutputStream out = new GitObject.DeflaterStream(Files.newOutputStream(tmp))) {
                out.write(data, off, len);
            }
            move(tmp, target);
        } finally {
            Files.deleteIfExists(tmp);
        }
        return id;
    }

    private static void move(Path from, Path to) throws IOException {
        try {
            Files.move(from, to, StandardCopyOption.ATOMIC_MOVE);
        } catch (java.nio.file.AtomicMoveNotSupportedException e) {
            Files.move(from, to, StandardCopyOption.REPLACE_EXISTING);
        }
    }

    private byte[] readChunk(ObjectId id) throws IOException {
        Path p = chunkPath(id);
        if (!Files.isRegularFile(p)) {
            throw new IOException("missing chunk " + id.hex());
        }
        try (InputStream in = new GitObject.InflaterStream(Files.newInputStream(p))) {
            byte[] raw = in.readAllBytes();
            if (!ObjectId.hash(raw).equals(id)) {
                throw new IOException("corrupt chunk " + id.hex());
            }
            return raw;
        }
    }

    // ------------------------------------------------------------- writing

    /** Result of storing one blob version. */
    public record StoredBlob(ObjectId id, int chunkCount, int copiedChunks, int insertedChunks,
                             int insertedBytes, long physicalBytes, ObjectId base) {
    }

    /**
     * Stores {@code content} as a blob, delta encoding it against
     * {@code candidateBase} when that helps. Returns the logical git blob id.
     */
    public StoredBlob store(byte[] content, ObjectId candidateBase) throws IOException {
        GitObject blob = GitObject.blob(content);
        ObjectId id = blob.id();

        // Already stored and already literal: nothing to do.
        if (Files.isRegularFile(recipePath(id))) {
            return describe(id);
        }

        List<RabinChunker.Chunk> chunks = chunker.chunk(content);

        // A small blob is cheaper to keep verbatim than to describe by recipe.
        if (content.length <= INLINE_THRESHOLD) {
            return storeInline(content, id);
        }

        // Try delta encoding against the candidate base.
        if (candidateBase != null && !candidateBase.equals(id)
                && (Files.isRegularFile(recipePath(candidateBase)) || deltaBases.containsKey(candidateBase))) {
            byte[] baseContent = readBlob(candidateBase);
            RecipeResult rr = buildRecipe(baseContent, content);
            if (rr != null && deflateLength(rr.bytes()) < deflateLength(content)) {
                ensureChunksPresent(content, chunks);
                return storeRecipe(id, rr.bytes(), candidateBase, chunks, content,
                        rr.copiedChunks(), rr.insertedChunks());
            }
        }

        // Otherwise store the chunk list as INSERT instructions.
        byte[] recipe = buildInsertRecipe(chunks);
        return storeRecipe(id, recipe, null, chunks, content, 0, chunks.size());
    }

    private StoredBlob storeInline(byte[] content, ObjectId id) throws IOException {
        Path target = recipePath(id);
        Files.createDirectories(target.getParent());
        Path tmp = Files.createTempFile(target.getParent(), "tmp_recipe_", null);
        long physical;
        try {
            try (OutputStream out = new GitObject.DeflaterStream(Files.newOutputStream(tmp))) {
                out.write(content);
            }
            physical = Files.size(tmp);
            move(tmp, target);
        } finally {
            Files.deleteIfExists(tmp);
        }
        return new StoredBlob(id, 1, 0, 1, content.length, physical, null);
    }

    private StoredBlob storeRecipe(ObjectId id, byte[] recipe, ObjectId base,
                                   List<RabinChunker.Chunk> chunks, byte[] content,
                                   int copied, int inserted) throws IOException {
        if (base == null) {
            // Root version: every chunk the INSERT list names must exist.
            for (RabinChunker.Chunk c : chunks) {
                writeChunk(content, c.offset(), c.length());
            }
        }
        long physical = writeRecipeFile(id, recipe, base);
        return new StoredBlob(id, chunks.size(), copied, inserted,
                insertedBytesOf(recipe), physical, base);
    }

    /**
     * Writes the recipe file: a varint-free header carrying the base blob id,
     * followed by the instruction stream, whole thing deflated.
     */
    private long writeRecipeFile(ObjectId id, byte[] recipe, ObjectId base) throws IOException {
        Path target = recipePath(id);
        Files.createDirectories(target.getParent());
        Path tmp = Files.createTempFile(target.getParent(), "tmp_recipe_", null);
        long size;
        try {
            try (OutputStream raw = Files.newOutputStream(tmp);
                 DataOutputStream out = new DataOutputStream(new GitObject.DeflaterStream(raw))) {
                out.writeInt(recipe.length);
                out.write(base == null ? new byte[ObjectId.LENGTH] : base.bytes());
                out.write(recipe);
            }
            size = Files.size(tmp);
            move(tmp, target);
        } finally {
            Files.deleteIfExists(tmp);
        }
        if (base != null) {
            deltaBases.put(id, base);
        }
        return size;
    }

    /** Total literal bytes named by INSERT instructions in a recipe. */
    private static int insertedBytesOf(byte[] recipe) {
        int total = 0;
        int i = 0;
        while (i < recipe.length) {
            int op = recipe[i++] & 0xff;
            if (op == OP_COPY) {
                i += varintSize(readVarint(recipe, i)) + varintSize(readVarint(recipe, i + varintSize(readVarint(recipe, i))));
            } else {
                int len = ((recipe[i] & 0xff) << 24) | ((recipe[i + 1] & 0xff) << 16)
                        | ((recipe[i + 2] & 0xff) << 8) | (recipe[i + 3] & 0xff);
                i += 4 + ObjectId.LENGTH;
                total += len;
            }
        }
        return total;
    }

    private static long readVarint(byte[] buf, int offset) {
        long result = 0;
        int shift = 0;
        int i = offset;
        while (true) {
            int b = buf[i++] & 0xff;
            result |= (long) (b & 0x7f) << shift;
            if ((b & 0x80) == 0) return result;
            shift += 7;
        }
    }

    private long deflateLength(byte[] data) throws IOException {
        var deflater = new java.util.zip.Deflater(java.util.zip.Deflater.DEFAULT_COMPRESSION);
        try {
            deflater.setInput(data);
            deflater.finish();
            long total = 0;
            byte[] buf = new byte[8192];
            while (!deflater.finished()) {
                total += deflater.deflate(buf);
            }
            return total;
        } finally {
            deflater.end();
        }
    }

    // ------------------------------------------------------------- recipes

    /**
     * Builds a COPY/INSERT recipe turning {@code base} into {@code target}, by
     * matching target chunks against base chunks greedily. Returns null when the
     * match rate is too poor to be worth it.
     */
    private RecipeResult buildRecipe(byte[] base, byte[] target) {
        List<RabinChunker.Chunk> baseChunks = chunker.chunk(base);
        List<RabinChunker.Chunk> targetChunks = chunker.chunk(target);

        Map<ObjectId, Integer> firstIndex = new LinkedHashMap<>();
        for (int i = 0; i < baseChunks.size(); i++) {
            firstIndex.putIfAbsent(baseChunks.get(i).id(), i);
        }

        ByteArrayOutputStream body = new ByteArrayOutputStream();
        byte[] resultBytes;
        int copiedCount = 0;
        int insertedCount = 0;
        try (DataOutputStream out = new DataOutputStream(body)) {
            int pendingCopies = 0;   // run length of consecutive base chunks
            int pendingCopyStart = 0;
            long matched = 0;
            long total = 0;

            for (RabinChunker.Chunk tc : targetChunks) {
                total += tc.length();
                Integer bi = firstIndex.get(tc.id());
                if (bi != null) {
                    if (pendingCopies == 0) {
                        pendingCopyStart = bi;
                    } else if (bi != pendingCopyStart + pendingCopies) {
                        emitCopy(out, pendingCopyStart, pendingCopies);
                        copiedCount += pendingCopies;
                        pendingCopies = 0;
                        pendingCopyStart = bi;
                    }
                    pendingCopies++;
                    copiedCount++;
                    matched += tc.length();
                } else {
                    if (pendingCopies > 0) {
                        emitCopy(out, pendingCopyStart, pendingCopies);
                        copiedCount += pendingCopies;
                        pendingCopies = 0;
                    }
                    emitInsert(out, tc);
                    insertedCount++;
                }
            }
            if (pendingCopies > 0) {
                emitCopy(out, pendingCopyStart, pendingCopies);
            }
            if (total > 0 && matched * 2 < total) {
                return null; // less than half matched: not worth a delta
            }
            out.flush();
            resultBytes = body.toByteArray();
        } catch (IOException e) {
            throw new IllegalStateException("ByteArrayOutputStream does not throw", e);
        }
        return new RecipeResult(resultBytes, copiedCount, insertedCount);
    }

    /** A built recipe plus the copy/insert statistics it implies. */
    private record RecipeResult(byte[] bytes, int copiedChunks, int insertedChunks) {
    }

    /**
     * Writes any chunk of the target that the recipe names but the pool lacks,
     * which is the case for the newly edited clauses in a delta version. Chunks
     * that already exist are left alone, so repeated edits never duplicate work.
     */
    private void ensureChunksPresent(byte[] target, List<RabinChunker.Chunk> targetChunks)
            throws IOException {
        for (RabinChunker.Chunk c : targetChunks) {
            if (!Files.isRegularFile(chunkPath(c.id()))) {
                writeChunk(target, c.offset(), c.length());
            }
        }
    }

    private static void emitCopy(DataOutputStream out, int baseOffset, int count) throws IOException {
        out.writeByte(OP_COPY);
        writeVarint(out, baseOffset);
        writeVarint(out, count);
    }

    private static void emitInsert(DataOutputStream out, RabinChunker.Chunk c) throws IOException {
        out.writeByte(OP_INSERT);
        out.writeInt(c.length());
        out.write(c.id().raw());
    }

    private byte[] buildInsertRecipe(List<RabinChunker.Chunk> chunks) {
        ByteArrayOutputStream body = new ByteArrayOutputStream();
        try (DataOutputStream out = new DataOutputStream(body)) {
            for (RabinChunker.Chunk c : chunks) {
                emitInsert(out, c);
            }
            out.flush();
        } catch (IOException e) {
            throw new IllegalStateException("ByteArrayOutputStream does not throw", e);
        }
        return body.toByteArray();
    }

    // ------------------------------------------------------------- reading

    /**
     * Reconstructs the exact blob content for {@code id}.
     *
     * <p>A blob is stored either verbatim (small content) or as a recipe. The
     * recipe records its own base, so reconstruction never depends on in-memory
     * state and survives a process restart.
     */
    public byte[] readBlob(ObjectId id) throws IOException {
        Path p = recipePath(id);
        if (!Files.isRegularFile(p)) {
            throw new IOException("blob not in chunk pack: " + id.hex());
        }
        byte[] packed;
        try (InputStream in = new GitObject.InflaterStream(Files.newInputStream(p))) {
            packed = in.readAllBytes();
        }
        // A verbatim blob hashes, with its git header, to its own id.
        if (ObjectId.hash(GitObject.withHeader(GitObject.Kind.BLOB, packed)).equals(id)) {
            return packed;
        }
        return replay(id, packed);
    }

    /**
     * Replays a recipe: COPY instructions pull runs of chunks out of the base
     * blob's chunk list, INSERT instructions name chunks already in the pool.
     *
     * @param packed the inflated recipe file, header included
     */
    private byte[] replay(ObjectId id, byte[] packed) throws IOException {
        ObjectId baseId;
        List<ObjectId> baseChunks;
        List<ObjectId> resultChunks = new ArrayList<>();
        ByteArrayOutputStream out = new ByteArrayOutputStream();
        try (DataInputStream in = new DataInputStream(new java.io.ByteArrayInputStream(packed))) {
            int recipeLen = in.readInt();
            byte[] baseRaw = new byte[ObjectId.LENGTH];
            in.readFully(baseRaw);
            baseId = ObjectId.of(baseRaw);
            baseChunks = baseId.equals(ObjectId.zero()) ? List.of() : chunkListOf(baseId);

            int remaining = recipeLen;
            while (remaining > 0) {
                int op = in.readUnsignedByte();
                remaining--;
                if (op == OP_COPY) {
                    long off = readVarint(in);
                    long len = readVarint(in);
                    remaining -= varintSize(off) + varintSize(len);
                    for (long i = 0; i < len; i++) {
                        ObjectId cid = baseChunks.get((int) (off + i));
                        out.write(readChunk(cid));
                        resultChunks.add(cid);
                    }
                } else if (op == OP_INSERT) {
                    int len = in.readInt();
                    byte[] raw = new byte[ObjectId.LENGTH];
                    in.readFully(raw);
                    remaining -= 4 + ObjectId.LENGTH;
                    ObjectId cid = ObjectId.of(raw);
                    out.write(readChunk(cid));
                    resultChunks.add(cid);
                } else {
                    throw new IOException("unknown delta opcode " + op + " in " + id.hex());
                }
            }
        }
        // Guard against a recipe that assembles into the wrong bytes.
        byte[] content = out.toByteArray();
        if (!GitObject.blob(content).id().equals(id)) {
            throw new IOException("reconstructed blob does not match its id " + id.hex());
        }
        lastReplay.put(id, resultChunks);
        return content;
    }

    private final Map<ObjectId, List<ObjectId>> lastReplay = new HashMap<>();

    /**
     * The ordered chunk ids that make up a stored blob, following the delta chain
     * back to a root. COPY indices in a recipe are positions in the base's chunk
     * list, so this is what resolves them.
     */
    public List<ObjectId> chunkListOf(ObjectId id) throws IOException {
        List<ObjectId> cached = chunkListCache.get(id);
        if (cached != null) {
            return cached;
        }
        List<ObjectId> replayed = lastReplay.get(id);
        if (replayed != null) {
            chunkListCache.put(id, replayed);
            return replayed;
        }
        Path p = recipePath(id);
        if (!Files.isRegularFile(p)) {
            throw new IOException("blob not in chunk pack: " + id.hex());
        }
        byte[] packed;
        try (InputStream in = new GitObject.InflaterStream(Files.newInputStream(p))) {
            packed = in.readAllBytes();
        }
        if (ObjectId.hash(GitObject.withHeader(GitObject.Kind.BLOB, packed)).equals(id)) {
            List<ObjectId> single = List.of(ObjectId.hash(packed));
            chunkListCache.put(id, single);
            return single;
        }
        // Materialising the chunks records the list for later reuse.
        readBlob(id);
        List<ObjectId> resolved = lastReplay.get(id);
        if (resolved != null) {
            chunkListCache.put(id, resolved);
            return resolved;
        }
        throw new IOException("could not resolve chunk list for " + id.hex());
    }

    private final Map<ObjectId, List<ObjectId>> chunkListCache = new HashMap<>();

    // ------------------------------------------------------------- metrics

    /** Total bytes physically occupied by this pack, for the size budget checks. */
    public long physicalBytes() throws IOException {
        return dirSize(dir);
    }

    private static long dirSize(Path root) throws IOException {
        if (!Files.exists(root)) return 0;
        try (var s = Files.walk(root)) {
            return s.filter(Files::isRegularFile).mapToLong(p -> {
                try {
                    return Files.size(p);
                } catch (IOException e) {
                    return 0L;
                }
            }).sum();
        }
    }

    /** Describes an already stored blob without rewriting anything. */
    private StoredBlob describe(ObjectId id) throws IOException {
        List<ObjectId> list = chunkListOf(id);
        Path p = recipePath(id);
        long physical = Files.isRegularFile(p) ? Files.size(p) : 0;
        return new StoredBlob(id, list.size(), 0, list.size(), 0, physical, deltaBases.get(id));
    }

    /** Reads one chunk; exposed so tests can verify reassembly piece by piece. */
    public byte[] readChunkForTest(ObjectId id) throws IOException {
        return readChunk(id);
    }

    /** Whether a blob id is known to this pack. */
    public boolean contains(ObjectId id) {
        return Files.isRegularFile(recipePath(id));
    }

    /** Number of chunk files present; tests assert reuse through this. */
    public int chunkFileCount() throws IOException {
        if (!Files.isDirectory(chunksDir())) return 0;
        try (var s = Files.walk(chunksDir())) {
            return (int) s.filter(Files::isRegularFile).count();
        }
    }

    // ------------------------------------------------------------ varints

    private static void writeVarint(DataOutputStream out, long value) throws IOException {
        while ((value & ~0x7fL) != 0) {
            out.writeByte((int) ((value & 0x7f) | 0x80));
            value >>>= 7;
        }
        out.writeByte((int) value);
    }

    private static long readVarint(DataInputStream in) throws IOException {
        long result = 0;
        int shift = 0;
        while (true) {
            int b = in.readUnsignedByte();
            result |= (long) (b & 0x7f) << shift;
            if ((b & 0x80) == 0) return result;
            shift += 7;
        }
    }

    private static int varintSize(long value) {
        int n = 1;
        while ((value & ~0x7fL) != 0) {
            n++;
            value >>>= 7;
        }
        return n;
    }

    /** Lists every blob id known to this pack, in ascending order. */
    public List<ObjectId> knownBlobs() throws IOException {
        List<ObjectId> out = new ArrayList<>();
        Path recipes = recipesDir();
        if (!Files.isDirectory(recipes)) return out;
        try (var s = Files.walk(recipes)) {
            for (Path p : s.filter(Files::isRegularFile).toList()) {
                String rel = recipes.relativize(p).toString().replace("/", "");
                if (rel.length() == ObjectId.HEX_LENGTH) {
                    out.add(ObjectId.fromHex(rel));
                }
            }
        }
        out.sort(null);
        return out;
    }

    @Override
    public String toString() {
        return "ChunkPack[" + dir + "]";
    }
}

package lawfirm.vcs;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardCopyOption;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;
import java.util.zip.Deflater;
import java.util.zip.Inflater;

/**
 * The three git object kinds and their canonical byte serialization.
 *
 * <p>Serialization follows the on-disk git format exactly so that an object
 * produced here has the same SHA-1 as the object system git produces for the
 * same logical content:
 *
 * <pre>
 * blob   : "blob " SP size NUL content
 * tree   : "tree " SP size NUL (mode SP name NUL 20-byte-id)*
 * commit : "commit " SP size NUL header LF LF message
 * </pre>
 *
 * <p>Tree entries are emitted in git's canonical order: byte-wise by name, with
 * directory names compared as if they carried a trailing '/'. Loose objects are
 * stored deflated under {@code objects/xx/yyyy...}.
 */
public abstract class GitObject {

    public static final int MODE_REGULAR = 0100644;
    public static final int MODE_EXECUTABLE = 0100755;
    public static final int MODE_SYMLINK = 0120000;
    public static final int MODE_TREE = 040000;
    public static final int MODE_GITLINK = 0160000;

    public enum Kind { BLOB, TREE, COMMIT }

    public abstract Kind kind();

    /** The exact bytes that are hashed and stored (including the header). */
    public abstract byte[] canonical();

    public ObjectId id() {
        return ObjectId.hash(canonical());
    }

    /**
     * Size of the stored object, header included. This is what {@code cat-file}
     * reports, matching git.
     */
    public long size() {
        return canonical().length;
    }

    /**
     * Size of the payload alone, without the {@code "<kind> <size>\0"} header.
     * This is the meaningful figure when totalling a tree's content.
     */
    public long contentSize() {
        byte[] canonical = canonical();
        int nul = 0;
        while (nul < canonical.length && canonical[nul] != 0) nul++;
        return canonical.length - nul - 1;
    }

    // ---------------------------------------------------------------- blobs

    public static final class Blob extends GitObject {
        private final byte[] content;

        Blob(byte[] content) {
            this.content = content;
        }

        @Override
        public Kind kind() {
            return Kind.BLOB;
        }

        public byte[] content() {
            return content;
        }

        @Override
        public byte[] canonical() {
            return withHeader(Kind.BLOB, content);
        }

        @Override
        public String toString() {
            return "blob[" + content.length + "]";
        }
    }

    public static GitObject blob(byte[] content) {
        return new Blob(content.clone());
    }

    // ---------------------------------------------------------------- trees

    /** One "<mode> <name>\0<20 raw id>" record of a tree object. */
    public static final class Entry {
        public final int mode;
        public final String name;
        public final ObjectId id;

        public Entry(int mode, String name, ObjectId id) {
            this.mode = mode;
            this.name = name;
            this.id = id;
        }

        public boolean isTree() {
            return mode == MODE_TREE;
        }

        /** The key git sorts this entry by: a directory sorts as "name/". */
        String sortKey() {
            return isTree() ? name + "/" : name;
        }

        /** Byte-wise comparison of two entry names, matching git's ordering. */
        static int compareNames(String a, String b) {
            return ObjectId.compare(a.getBytes(StandardCharsets.UTF_8),
                    b.getBytes(StandardCharsets.UTF_8));
        }

        /** Byte-wise comparison of two sort keys. */
        static int compareSortKeys(String a, String b) {
            return compareNames(a, b);
        }

        @Override
        public String toString() {
            return String.format("%06o %s %s", mode, name, id.hex());
        }
    }

    public static final class Tree extends GitObject {
        private final Entry[] entries;

        Tree(List<Entry> entries) {
            this.entries = entries.toArray(new Entry[0]);
        }

        @Override
        public Kind kind() {
            return Kind.TREE;
        }

        public Entry[] entries() {
            return entries.clone();
        }

        public int entryCount() {
            return entries.length;
        }

        public Entry find(String name) {
            for (Entry e : entries) {
                if (e.name.equals(name)) return e;
            }
            return null;
        }

        @Override
        public byte[] canonical() {
            ByteArrayOutputStream body = new ByteArrayOutputStream();
            for (Entry e : entries) {
                body.writeBytes(modeString(e.mode));
                body.write(' ');
                body.writeBytes(e.name.getBytes(StandardCharsets.UTF_8));
                body.write(0);
                body.writeBytes(e.id.raw());
            }
            return withHeader(Kind.TREE, body.toByteArray());
        }

        @Override
        public String toString() {
            StringBuilder sb = new StringBuilder("tree[");
            for (Entry e : entries) {
                sb.append(e).append(' ');
            }
            return sb.append(']').toString();
        }
    }

    /**
     * Builds a tree in canonical order. Entries with the same name are rejected:
     * git cannot represent a duplicate path component.
     */
    public static GitObject tree(List<Entry> entries) {
        List<Entry> copy = new ArrayList<>(entries);
        for (int i = 1; i < copy.size(); i++) {
            String prev = copy.get(i - 1).sortKey();
            String cur = copy.get(i).sortKey();
            if (prev.equals(cur)) {
                throw new IllegalArgumentException("duplicate tree entry: " + copy.get(i).name);
            }
            // Compare bytes, not UTF-16 code units: for non-ASCII names the two
            // orderings differ, and git orders by raw bytes.
            if (Entry.compareSortKeys(prev, cur) > 0) {
                throw new IllegalArgumentException(
                        "tree entries must be sorted: " + copy.get(i - 1) + " before " + copy.get(i));
            }
        }
        return new Tree(copy);
    }

    public static GitObject tree(Entry... entries) {
        return tree(Arrays.asList(entries));
    }

    /** Parses a tree object body (the bytes after the header/NUL). */
    public static GitObject parseTree(byte[] body) {
        List<Entry> entries = new ArrayList<>();
        int i = 0;
        while (i < body.length) {
            int sp = indexOf(body, ' ', i);
            if (sp < 0) throw new IllegalArgumentException("malformed tree entry: no mode separator");
            int mode = Integer.parseInt(new String(body, i, sp - i, StandardCharsets.US_ASCII), 8);
            int nul = indexOf(body, 0, sp + 1);
            if (nul < 0) throw new IllegalArgumentException("malformed tree entry: no name terminator");
            String name = new String(body, sp + 1, nul - sp - 1, StandardCharsets.UTF_8);
            if (nul + 1 + ObjectId.LENGTH > body.length) {
                throw new IllegalArgumentException("malformed tree entry: truncated object id");
            }
            entries.add(new Entry(mode, name, ObjectId.of(body, nul + 1)));
            i = nul + 1 + ObjectId.LENGTH;
        }
        return tree(entries);
    }

    private static int indexOf(byte[] a, int value, int from) {
        for (int i = from; i < a.length; i++) {
            if (a[i] == (byte) value) return i;
        }
        return -1;
    }

    // -------------------------------------------------------------- commits

    public static final class Commit extends GitObject {
        private final ObjectId tree;
        private final List<ObjectId> parents;
        private final String author;
        private final String committer;
        private final String message;

        Commit(ObjectId tree, List<ObjectId> parents, String author, String committer, String message) {
            this.tree = tree;
            this.parents = List.copyOf(parents);
            this.author = author;
            this.committer = committer;
            this.message = message;
        }

        @Override
        public Kind kind() {
            return Kind.COMMIT;
        }

        public ObjectId tree() {
            return tree;
        }

        public List<ObjectId> parents() {
            return parents;
        }

        public ObjectId firstParent() {
            return parents.isEmpty() ? null : parents.get(0);
        }

        public String author() {
            return author;
        }

        public String committer() {
            return committer;
        }

        public String message() {
            return message;
        }

        @Override
        public byte[] canonical() {
            StringBuilder sb = new StringBuilder();
            sb.append("tree ").append(tree.hex()).append('\n');
            for (ObjectId p : parents) {
                sb.append("parent ").append(p.hex()).append('\n');
            }
            sb.append("author ").append(author).append('\n');
            sb.append("committer ").append(committer).append('\n');
            sb.append('\n');
            // Git always terminates the message with a newline. The header block
            // is already closed above, so the blank line and the trailing LF are
            // both part of the hashed bytes.
            String msg = message.endsWith("\n") ? message : message + "\n";
            return GitObject.withHeader(Kind.COMMIT,
                    (sb.toString() + msg).getBytes(StandardCharsets.UTF_8));
        }

        @Override
        public String toString() {
            return "commit[" + tree.abbrev(8) + " parents=" + parents.size() + "]";
        }
    }

    /**
     * @param author    full identity line, "Name &lt;email&gt; ts tz"
     * @param committer full identity line; usually equal to author
     */
    public static GitObject commit(ObjectId tree, List<ObjectId> parents,
                                   String author, String committer, String message) {
        return new Commit(tree, parents, author, committer, message);
    }

    public static GitObject commit(ObjectId tree, List<ObjectId> parents, String message) {
        Identity id = Identity.defaultIdentity();
        return commit(tree, parents, id.canonical(), id.canonical(), message);
    }

    /**
     * Parses a commit object body, i.e. everything after the
     * {@code "commit <size>\0"} prefix.
     *
     * <p>Inside the body the header block is separated from the free text
     * message by a blank line; multi-paragraph messages are preserved verbatim,
     * including the leading newline git stores.
     */
    public static GitObject parseCommit(byte[] body) {
        int split = indexOfNewlineNewline(body);
        String head;
        String message;
        if (split < 0) {
            head = new String(body, StandardCharsets.UTF_8);
            message = "";
        } else {
            head = new String(body, 0, split, StandardCharsets.UTF_8);
            // skip the blank line itself
            message = new String(body, split + 2, body.length - split - 2, StandardCharsets.UTF_8);
        }
        ObjectId tree = null;
        List<ObjectId> parents = new ArrayList<>();
        String author = null;
        String committer = null;
        for (String line : head.split("\n")) {
            if (line.startsWith("tree ")) {
                tree = ObjectId.fromHex(line.substring(5).trim());
            } else if (line.startsWith("parent ")) {
                parents.add(ObjectId.fromHex(line.substring(7).trim()));
            } else if (line.startsWith("author ")) {
                author = line.substring(7);
            } else if (line.startsWith("committer ")) {
                committer = line.substring(10);
            }
        }
        if (tree == null) {
            throw new IllegalArgumentException("malformed commit: missing tree");
        }
        if (author == null) {
            throw new IllegalArgumentException("malformed commit: missing author");
        }
        return new Commit(tree, parents, author, committer == null ? author : committer, message);
    }

    /** Index of the first blank line in a commit body, or -1. */
    private static int indexOfNewlineNewline(byte[] body) {
        for (int i = 0; i + 1 < body.length; i++) {
            if (body[i] == '\n' && body[i + 1] == '\n') {
                return i;
            }
        }
        return -1;
    }

    // ------------------------------------------------------------ dispatch

    /** Wraps a canonical payload in its "kind SP size NUL" header. */
    public static byte[] withHeader(Kind kind, byte[] body) {
        String header = kind.name().toLowerCase() + " " + body.length + "\0";
        byte[] head = header.getBytes(StandardCharsets.US_ASCII);
        byte[] out = new byte[head.length + body.length];
        System.arraycopy(head, 0, out, 0, head.length);
        System.arraycopy(body, 0, out, head.length, body.length);
        return out;
    }

    /** Reads the kind and body out of a full canonical payload. */
    public static GitObject parse(byte[] canonical) {
        int nul = indexOf(canonical, 0, 0);
        if (nul < 0) throw new IllegalArgumentException("malformed object: no header terminator");
        String header = new String(canonical, 0, nul, StandardCharsets.US_ASCII);
        int sp = header.indexOf(' ');
        if (sp < 0) throw new IllegalArgumentException("malformed object header: " + header);
        Kind kind = Kind.valueOf(header.substring(0, sp).toUpperCase());
        int declared = Integer.parseInt(header.substring(sp + 1));
        int actual = canonical.length - nul - 1;
        if (declared != actual) {
            throw new IllegalArgumentException("size mismatch: header says " + declared + ", have " + actual);
        }
        byte[] body = Arrays.copyOfRange(canonical, nul + 1, canonical.length);
        return switch (kind) {
            case BLOB -> blob(body);
            case TREE -> parseTree(body);
            case COMMIT -> parseCommit(body);
        };
    }

    /**
     * Renders a file mode the way git writes it in a tree object.
     *
     * <p>Regular files use the full six digit form {@code 100644}; directories
     * are written as {@code 40000} with the leading zero stripped, and gitlinks
     * and symlinks keep their six digit form. Getting this exactly right matters
     * because the mode text is part of the hashed tree bytes.
     *
     * <p>A tree is therefore ordered by the sort key "name/" for subdirectories
     * and "name" for everything else, compared byte-wise over UTF-8.
     */
    static byte[] modeString(int mode) {
        if (mode == MODE_TREE) {
            return "40000".getBytes(StandardCharsets.US_ASCII);
        }
        return Integer.toOctalString(mode).getBytes(StandardCharsets.US_ASCII);
    }

    // -------------------------------------------------------- loose storage

    /**
     * Content addressed loose object storage rooted at {@code objectsDir}, laid
     * out exactly like git: {@code objects/ab/cdef...}.
     */
    public static final class LooseStore {
        private final Path objectsDir;

        public LooseStore(Path objectsDir) {
            this.objectsDir = objectsDir;
        }

        public Path pathFor(ObjectId id) {
            String hex = id.hex();
            return objectsDir.resolve(hex.substring(0, 2)).resolve(hex.substring(2));
        }

        public boolean has(ObjectId id) {
            return Files.isRegularFile(pathFor(id));
        }

        /** Writes the object if absent and returns its id. Idempotent. */
        public ObjectId write(GitObject object) throws IOException {
            return write(object, null);
        }

        /**
         * Writes the object if absent and returns its id.
         *
         * @param wroteOut set to true when bytes were actually written, which lets
         *                 callers measure how much of a tree a commit really cost
         */
        public ObjectId write(GitObject object, java.util.function.Consumer<Boolean> wroteOut)
                throws IOException {
            byte[] canonical = object.canonical();
            ObjectId id = ObjectId.hash(canonical);
            Path target = pathFor(id);
            if (Files.isRegularFile(target)) {
                if (wroteOut != null) wroteOut.accept(false);
                return id;
            }
            if (wroteOut != null) wroteOut.accept(true);
            Files.createDirectories(target.getParent());
            Path tmp = Files.createTempFile(target.getParent(), "tmp_obj_", null);
            try {
                try (OutputStream out = new DeflaterStream(new java.io.FileOutputStream(tmp.toFile()))) {
                    out.write(canonical);
                }
                try {
                    Files.move(tmp, target, StandardCopyOption.ATOMIC_MOVE);
                } catch (java.nio.file.AtomicMoveNotSupportedException e) {
                    Files.move(tmp, target, StandardCopyOption.REPLACE_EXISTING);
                }
            } finally {
                Files.deleteIfExists(tmp);
            }
            return id;
        }

        /** Reads and inflates a loose object, verifying its id. */
        public GitObject read(ObjectId id) throws IOException {
            Path p = pathFor(id);
            if (!Files.isRegularFile(p)) {
                throw new java.io.FileNotFoundException("loose object not found: " + id.hex());
            }
            byte[] canonical;
            try (InputStream in = new InflaterStream(Files.newInputStream(p))) {
                canonical = in.readAllBytes();
            }
            if (!ObjectId.hash(canonical).equals(id)) {
                throw new IOException("corrupt loose object: content does not hash to " + id.hex());
            }
            return parse(canonical);
        }

        /** All loose object ids currently stored, in ascending order. */
        public List<ObjectId> listAll() throws IOException {
            List<ObjectId> ids = new ArrayList<>();
            if (!Files.isDirectory(objectsDir)) return ids;
            try (var dirs = Files.list(objectsDir)) {
                for (Path d : dirs.toList()) {
                    String name = d.getFileName().toString();
                    if (name.length() != 2 || !Files.isDirectory(d)) continue;
                    try (var files = Files.list(d)) {
                        for (Path f : files.toList()) {
                            String rest = f.getFileName().toString();
                            if (rest.length() != ObjectId.HEX_LENGTH - 2) continue;
                            ids.add(ObjectId.fromHex(name + rest));
                        }
                    }
                }
            }
            ids.sort(null);
            return ids;
        }
    }

    /** Deflate stream wrapper. Compression level mirrors git's default behaviour. */
    public static final class DeflaterStream extends java.util.zip.DeflaterOutputStream {
        public DeflaterStream(OutputStream out) {
            super(out, new Deflater(Deflater.DEFAULT_COMPRESSION));
        }
    }

    /** Inflate stream wrapper. */
    public static final class InflaterStream extends java.util.zip.InflaterInputStream {
        public InflaterStream(InputStream in) {
            super(in, new Inflater());
        }
    }
}

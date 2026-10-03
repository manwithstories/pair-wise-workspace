package io.corp.artifactres.core;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.TreeMap;

/**
 * Reads local artifact metadata from a plain-text directory. No network access.
 *
 * <p>One file per package, named {@code <groupId>__<artifactId>.dep}, where underscores in
 * the coordinates become a double underscore. Each non-empty, non-comment line declares
 * one version and its edges:
 *
 * <pre>
 *   # com.acme core
 *   version 1.4.0
 *   version 1.5.0 -&gt; com.acme:io [1.4,2.0)
 *   version 1.5.0 ! com.acme:legacy
 * </pre>
 *
 * <p>The directory is read in sorted file-name order so a given directory always yields
 * the same repository instance.
 */
public final class MetadataDir {

    private MetadataDir() {
    }

    /** Load every {@code *.dep} file under {@code dir} into a repository map. */
    public static TreeMap<String, Resolver.Module> load(Path dir) throws IOException {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        if (!Files.isDirectory(dir)) {
            throw new IOException("metadata directory not found: " + dir);
        }
        List<Path> files = new ArrayList<>();
        try (var stream = Files.list(dir)) {
            stream.filter(p -> p.getFileName().toString().endsWith(".dep")).forEach(files::add);
        }
        files.sort(java.util.Comparator.comparing(p -> p.getFileName().toString()));
        for (Path f : files) {
            String name = f.getFileName().toString();
            String id = packageIdFromFileName(name);
            Resolver.Module.Builder b = Resolver.Module.builder(id);
            for (String raw : Files.readAllLines(f, StandardCharsets.UTF_8)) {
                String line = raw.trim();
                if (line.isEmpty() || line.startsWith("#") || !line.startsWith("version ")) {
                    continue;
                }
                String rest = line.substring("version ".length()).trim();

                // Exclusion: "version 1.0 ! target" -- the '!' follows the version.
                int bang = rest.indexOf('!');
                if (bang >= 0) {
                    String ver = rest.substring(0, bang).trim();
                    String target = rest.substring(bang + 1).trim();
                    if (target.isEmpty()) {
                        throw new IOException("exclusion without a target in " + name + ": " + line);
                    }
                    b.exclude(ver, target);
                    continue;
                }

                // Dependency: "version 1.0 -> target [range]". A bare "version 1.0"
                // declares a version with no direct edges.
                int arrow = rest.indexOf("->");
                if (arrow < 0) {
                    b.version(rest);
                    continue;
                }
                String ver = rest.substring(0, arrow).trim();
                String spec = rest.substring(arrow + 2).trim();
                int sp = spec.indexOf(' ');
                if (sp < 0) {
                    throw new IOException("dependency without a version range in " + name
                            + ": " + line);
                }
                b.version(ver, spec.substring(0, sp).trim(), spec.substring(sp + 1).trim());
            }
            repo.put(id, b.build());
        }
        return repo;
    }

    /** {@code com.acme__core.dep} -> {@code com.acme:core}. */
    static String packageIdFromFileName(String fileName) {
        String stem = fileName.endsWith(".dep")
                ? fileName.substring(0, fileName.length() - 4)
                : fileName;
        int sep = stem.lastIndexOf("__");
        if (sep < 0) {
            throw new IllegalArgumentException(
                    "metadata file name must be '<groupId>__<artifactId>.dep': " + fileName);
        }
        return stem.substring(0, sep) + ":" + stem.substring(sep + 2);
    }
}

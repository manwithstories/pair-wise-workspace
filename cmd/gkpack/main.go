package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"example.com/gkpack/internal/delta"
	"example.com/gkpack/internal/pack"
	"example.com/gkpack/internal/verify"
	"example.com/gkpack/internal/versiondb"
)

const (
	exitOK           = 0
	exitOperation    = 1
	exitUsage        = 2
	exitVerification = 3
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		writeError(stderr, "usage", "missing command", nil)
		printUsage(stdout)
		return exitUsage
	}
	switch args[0] {
	case "build":
		return runBuild(args[1:], stdout, stderr)
	case "delta":
		return runDelta(args[1:], stdout, stderr)
	case "apply":
		return runApply(args[1:], stdout, stderr)
	case "apply-path":
		return runApplyPath(args[1:], stdout, stderr)
	case "verify":
		return runVerify(args[1:], stdout, stderr)
	case "version":
		return runVersion(args[1:], stdout, stderr)
	case "root":
		return runRoot(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return exitOK
	default:
		writeError(stderr, "usage", fmt.Sprintf("unknown command %q", args[0]), nil)
		printUsage(stdout)
		return exitUsage
	}
}

func runBuild(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fsrc := fs.String("src", "", "source resource directory")
	farchive := fs.String("archive", "", "output archive path")
	fversion := fs.String("version", "", "version name")
	fworkers := fs.Int("workers", 0, "compression workers (0 = GOMAXPROCS)")
	if err := fs.Parse(args); err != nil {
		return usageError(stderr, err)
	}
	if *fsrc == "" || *farchive == "" {
		return usageError(stderr, errors.New("build requires --src and --archive"))
	}
	res, err := pack.Build(pack.BuildOptions{
		SourceDir:   *fsrc,
		ArchivePath: *farchive,
		Version:     *fversion,
		Workers:     *fworkers,
	})
	if err != nil {
		return operationError(stderr, err)
	}
	ratio := 0.0
	if res.PackedBytes > 0 {
		ratio = float64(res.RawBytes) / float64(res.PackedBytes)
	}
	fmt.Fprintf(stdout, "%s raw=%s packed=%s 压缩比=%.2f 块数=%d\n",
		res.Version, pack.FormatMiB(res.RawBytes), pack.FormatMiB(uint64(res.PackedBytes)), ratio, res.UniqueBlocks)
	return exitOK
}

func runDelta(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("delta", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fbase := fs.String("base", "", "baseline archive")
	ftarget := fs.String("target", "", "target archive")
	fpatch := fs.String("patch", "", "output patch")
	fverbose := fs.Bool("verbose", false, "print patch compression ratio")
	if err := fs.Parse(args); err != nil {
		return usageError(stderr, err)
	}
	if *fbase == "" || *ftarget == "" || *fpatch == "" {
		return usageError(stderr, errors.New("delta requires --base, --target, and --patch"))
	}
	res, err := delta.Create(delta.CreateOptions{BaseArchive: *fbase, TargetArchive: *ftarget, PatchPath: *fpatch})
	if err != nil {
		return operationError(stderr, err)
	}
	fmt.Fprintf(stdout, "增量 %s→%s 变更块=%d/%d 复用率=%.1f%% 补丁=%s\n",
		res.BaseVersion, res.TargetVersion, res.ChangedBlocks, res.TargetBlocks,
		res.ReusePercent, pack.FormatMiB(uint64(res.PatchBytes)))
	if *fverbose {
		fmt.Fprintf(stdout, "补丁压缩比=%.2f 新增原始块=%s 正向块=%d 反向块=%d\n",
			res.PatchRatio, pack.FormatMiB(res.NewRawBytes), res.ForwardChunks, res.ReverseChunks)
	}
	return exitOK
}

func runApply(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fbase := fs.String("base", "", "baseline archive")
	fpatch := fs.String("patch", "", "patch file")
	fout := fs.String("out", "", "output resource directory")
	freverse := fs.Bool("reverse", false, "apply the reverse direction")
	fexpect := fs.String("expect-version", "", "required target version")
	fnorescan := fs.Bool("no-root-rescan", false, "skip final root rescan (tests only)")
	if err := fs.Parse(args); err != nil {
		return usageError(stderr, err)
	}
	if *fbase == "" || *fpatch == "" || *fout == "" {
		return usageError(stderr, errors.New("apply requires --base, --patch, and --out"))
	}
	res, err := delta.Apply(delta.ApplyOptions{
		BaseArchive:    *fbase,
		PatchPath:      *fpatch,
		OutputDir:      *fout,
		Reverse:        *freverse,
		ExpectVersion:  *fexpect,
		SkipRootRescan: *fnorescan,
	})
	if err != nil {
		return operationError(stderr, err)
	}
	printApplyResult(stdout, res, *freverse, "")
	return exitOK
}

func printApplyResult(w io.Writer, res delta.ApplyResult, reverse bool, prefix string) {
	short := res.Root
	if len(short) > 8 {
		short = short[:8]
	}
	if reverse {
		fmt.Fprintf(w, "%s回退 %s→%s 校验通过 恢复=%d 文件 移除=%d 文件\n",
			prefix, res.BaseVersion, res.TargetVersion, res.Restored, res.Removed)
		return
	}
	fmt.Fprintf(w, "%s应用 %s+补丁 校验通过 root=%s\n", prefix, res.BaseVersion, short)
}

func runApplyPath(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("apply-path", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fdb := fs.String("db", "", "version database directory")
	ffrom := fs.String("from", "", "starting version")
	fto := fs.String("to", "", "target version")
	fout := fs.String("out", "", "output resource directory")
	if err := fs.Parse(args); err != nil {
		return usageError(stderr, err)
	}
	if *fdb == "" || *ffrom == "" || *fto == "" || *fout == "" {
		return usageError(stderr, errors.New("apply-path requires --db, --from, --to, and --out"))
	}
	db, err := versiondb.Open(*fdb)
	if err != nil {
		return operationError(stderr, err)
	}
	hops, err := db.Path(*ffrom, *fto)
	if err != nil {
		return operationError(stderr, err)
	}
	if len(hops) == 0 {
		fmt.Fprintf(stdout, "路径应用 %s→%s 校验通过 root=%s hops=0\n", *ffrom, *fto, shortRoot(db.Versions[*ffrom].Root))
		return exitOK
	}
	multi := len(hops) > 1
	for i, hop := range hops {
		from := db.Versions[hop.From]
		applyRes, err := delta.Apply(delta.ApplyOptions{
			BaseArchive:   from.Archive,
			PatchPath:     hop.Patch,
			OutputDir:     *fout,
			Reverse:       hop.Reverse,
			ExpectVersion: hop.To,
		})
		if err != nil {
			return operationError(stderr, fmt.Errorf("hop %d/%d %s->%s: %w", i+1, len(hops), hop.From, hop.To, err))
		}
		if multi {
			fmt.Fprintf(stdout, "跳 %d/%d %s→%s%s 校验通过 root=%s\n",
				i+1, len(hops), hop.From, hop.To, directionLabel(hop.Reverse), shortRoot(applyRes.Root))
		} else {
			printApplyResult(stdout, applyRes, hop.Reverse, "")
		}
	}
	if multi {
		fmt.Fprintf(stdout, "路径应用 %s→%s 校验通过 root=%s hops=%d\n",
			*ffrom, *fto, shortRoot(db.Versions[*fto].Root), len(hops))
	}
	return exitOK
}

func directionLabel(reverse bool) string {
	if reverse {
		return "(反向)"
	}
	return "(正向)"
}

func runVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	farchive := fs.String("archive", "", "archive to inspect")
	ftree := fs.String("tree", "", "extracted resource tree to inspect")
	if err := fs.Parse(args); err != nil {
		return usageError(stderr, err)
	}
	if *farchive == "" {
		return usageError(stderr, errors.New("verify requires --archive; --tree is optional"))
	}
	var res verify.Result
	var err error
	if *ftree != "" {
		res, err = verify.Both(*farchive, *ftree)
	} else {
		res, err = verify.Archive(*farchive)
	}
	if err != nil {
		writeError(stderr, "verification_error", err.Error(), nil)
		return exitVerification
	}
	fmt.Fprintln(stdout, res.FormatSummary())
	if res.FailedBlocks > 0 || res.FailedFiles > 0 {
		enc := json.NewEncoder(stderr)
		for _, issue := range res.Issues {
			_ = enc.Encode(struct {
				Level  string `json:"level"`
				Code   string `json:"code"`
				Path   string `json:"path"`
				Block  string `json:"block,omitempty"`
				Reason string `json:"reason"`
			}{"error", "verify_failed", issue.Path, issue.Block, issue.Reason})
		}
		_ = enc.Encode(map[string]any{
			"level":         "error",
			"code":          "verification_failed",
			"failed_blocks": res.FailedBlocks,
			"failed_files":  res.FailedFiles,
			"paths":         res.Paths,
		})
		return exitVerification
	}
	return exitOK
}

func runVersion(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageError(stderr, errors.New("version requires add, patch, list, or path"))
	}
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("version add", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fdb := fs.String("db", "", "version database directory")
		fname := fs.String("name", "", "version name")
		fbranch := fs.String("branch", "", "branch name")
		fparent := fs.String("parent", "", "parent version")
		farchive := fs.String("archive", "", "archive path")
		if err := fs.Parse(args[1:]); err != nil {
			return usageError(stderr, err)
		}
		if *fdb == "" || *fname == "" || *fbranch == "" || *farchive == "" {
			return usageError(stderr, errors.New("version add requires --db, --name, --branch, and --archive"))
		}
		archive, err := filepath.Abs(*farchive)
		if err != nil {
			return operationError(stderr, err)
		}
		db, err := versiondb.Open(*fdb)
		if err != nil {
			return operationError(stderr, err)
		}
		if err := db.AddVersion(versiondb.Version{Name: *fname, Branch: *fbranch, Parent: *fparent, Archive: archive}); err != nil {
			return operationError(stderr, err)
		}
		fmt.Fprintf(stdout, "version=%s branch=%s root=%s\n", *fname, *fbranch, shortRoot(db.Versions[*fname].Root))
		return exitOK
	case "patch":
		fs := flag.NewFlagSet("version patch", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fdb := fs.String("db", "", "version database directory")
		ffrom := fs.String("from", "", "base version")
		fto := fs.String("to", "", "target version")
		fpatch := fs.String("patch", "", "patch path")
		if err := fs.Parse(args[1:]); err != nil {
			return usageError(stderr, err)
		}
		if *fdb == "" || *ffrom == "" || *fto == "" || *fpatch == "" {
			return usageError(stderr, errors.New("version patch requires --db, --from, --to, and --patch"))
		}
		patchPath, err := filepath.Abs(*fpatch)
		if err != nil {
			return operationError(stderr, err)
		}
		db, err := versiondb.Open(*fdb)
		if err != nil {
			return operationError(stderr, err)
		}
		if err := db.AddPatch(*ffrom, *fto, patchPath); err != nil {
			return operationError(stderr, err)
		}
		fmt.Fprintf(stdout, "patch_registered %s→%s\n", *ffrom, *fto)
		return exitOK
	case "list":
		fs := flag.NewFlagSet("version list", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fdb := fs.String("db", "", "version database directory")
		if err := fs.Parse(args[1:]); err != nil {
			return usageError(stderr, err)
		}
		if *fdb == "" {
			return usageError(stderr, errors.New("version list requires --db"))
		}
		db, err := versiondb.Open(*fdb)
		if err != nil {
			return operationError(stderr, err)
		}
		for _, v := range db.VersionsSorted() {
			fmt.Fprintf(stdout, "%s branch=%s parent=%s root=%s archive=%s\n", v.Name, v.Branch, v.Parent, shortRoot(v.Root), v.Archive)
		}
		return exitOK
	case "path":
		fs := flag.NewFlagSet("version path", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fdb := fs.String("db", "", "version database directory")
		ffrom := fs.String("from", "", "starting version")
		fto := fs.String("to", "", "target version")
		if err := fs.Parse(args[1:]); err != nil {
			return usageError(stderr, err)
		}
		if *fdb == "" || *ffrom == "" || *fto == "" {
			return usageError(stderr, errors.New("version path requires --db, --from, and --to"))
		}
		db, err := versiondb.Open(*fdb)
		if err != nil {
			return operationError(stderr, err)
		}
		hops, err := db.Path(*ffrom, *fto)
		if err != nil {
			return operationError(stderr, err)
		}
		if len(hops) == 0 {
			fmt.Fprintf(stdout, "%s\n", *ffrom)
			return exitOK
		}
		var b strings.Builder
		for i, hop := range hops {
			if i == 0 {
				b.WriteString(hop.From)
			}
			b.WriteString(" -> ")
			b.WriteString(hop.To)
			b.WriteString(directionLabel(hop.Reverse))
		}
		fmt.Fprintln(stdout, b.String())
		return exitOK
	default:
		return usageError(stderr, fmt.Errorf("unknown version command %q", args[0]))
	}
}

func runRoot(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("root", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	farchive := fs.String("archive", "", "archive path")
	if err := fs.Parse(args); err != nil {
		return usageError(stderr, err)
	}
	if *farchive == "" {
		return usageError(stderr, errors.New("root requires --archive"))
	}
	reader, err := pack.Open(*farchive)
	if err != nil {
		return operationError(stderr, err)
	}
	defer reader.Close()
	fmt.Fprintln(stdout, reader.Root())
	return exitOK
}

func shortRoot(root string) string {
	if len(root) > 8 {
		return root[:8]
	}
	return root
}

func usageError(stderr io.Writer, err error) int {
	writeError(stderr, "usage", err.Error(), nil)
	return exitUsage
}

func operationError(stderr io.Writer, err error) int {
	writeError(stderr, "operation_failed", err.Error(), nil)
	return exitOperation
}

func writeError(w io.Writer, code, message string, extra map[string]any) {
	payload := map[string]any{
		"level":   "error",
		"code":    code,
		"message": message,
	}
	for k, v := range extra {
		payload[k] = v
	}
	_ = json.NewEncoder(w).Encode(payload)
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `gkpack - deterministic local block delta packs

Commands:
  gkpack build --src DIR --archive FILE [--version NAME] [--workers N]
  gkpack delta --base BASE.gkpack --target TARGET.gkpack --patch OUT.gkpatch [--verbose]
  gkpack apply --base BASE.gkpack --patch PATCH.gkpatch --out DIR [--reverse] [--expect-version V]
  gkpack verify --archive FILE [--tree DIR]
  gkpack version add --db DIR --name V --branch B [--parent P] --archive FILE
  gkpack version patch --db DIR --from V1 --to V2 --patch FILE
  gkpack version list --db DIR
  gkpack version path --db DIR --from V1 --to V2
  gkpack apply-path --db DIR --from V1 --to V2 --out DIR
  gkpack root --archive FILE

Exit codes: 0 success, 1 operation/integrity failure, 2 usage, 3 verification failure.`)
}

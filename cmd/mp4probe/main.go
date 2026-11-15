// mp4probe is an offline MP4 index and keyframe locator for forensic
// review of rail vehicle recordings. It parses box structure, builds
// per-sample indexes, locates landing keyframes and hashes samples;
// it never decodes, renders or contacts any external service.
package main

import (
	"fmt"
	"os"

	"mp4probe/internal/cli"
)

const usage = `mp4probe - offline MP4 index & keyframe locator

usage:
  mp4probe fixture --out DIR --cases a,b,c [--seed N] [--json]
  mp4probe index   --in FILE [--out INDEX.json] [--json]
  mp4probe seek    --in FILE --at HH:MM:SS.mmm [--track N] [--frames N] [--out R.json] [--json]
  mp4probe export  --in FILE --from TC --to TC [--track N] [--out M.json] [--json]
  mp4probe verify  --in FILE --index INDEX.json --expect M.json [--json]

exit codes: 0 ok, 2 usage, 3 box structure, 4 index consistency,
5 no keyframe/out of range, 6 hash mismatch, 7 I/O error
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return cli.ExitUsage
	}
	var err error
	switch args[0] {
	case "fixture":
		err = cli.RunFixture(args[1:])
	case "index":
		err = cli.RunIndex(args[1:])
	case "seek":
		err = cli.RunSeek(args[1:])
	case "export":
		err = cli.RunExport(args[1:])
	case "verify":
		err = cli.RunVerify(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n%s", args[0], usage)
		return cli.ExitUsage
	}
	if err != nil {
		cli.PrintError(os.Stderr, err)
		return cli.ExitCode(err)
	}
	return cli.ExitOK
}

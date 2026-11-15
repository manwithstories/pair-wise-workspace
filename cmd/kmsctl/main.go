// Command kmsctl is a fully local key-rotation, license and verification
// pipeline for encrypted content shards.
//
// Exit codes: 0 success, 1 verification failures, 2 parameter or IO
// error, 3 internal state inconsistency.
package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

const (
	exitOK = iota
	exitVerifyFailed
	exitUsage
	exitInternal
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitUsage)
	}
	var code int
	switch os.Args[1] {
	case "gen-fixture":
		code = cmdGenFixture(os.Args[2:])
	case "encrypt":
		code = cmdEncrypt(os.Args[2:])
	case "issue":
		code = cmdIssue(os.Args[2:])
	case "verify":
		code = cmdVerify(os.Args[2:])
	case "report":
		code = cmdReport(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		usage()
		code = exitUsage
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: kmsctl <gen-fixture|encrypt|issue|verify|report> [flags]")
}

func fail(code int, err error) int {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	return code
}

func printJSON(v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: marshal output: %v\n", err)
		return
	}
	fmt.Println(string(raw))
}

// loadMaster reads hex master key material from dir/master.key.
func loadMaster(dir string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "master.key"))
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(string(bytes.TrimSpace(raw)))
}

// summary is the standard JSON result of encrypt/verify/report.
type summary struct {
	Shards      int             `json:"shards"`
	Rotations   int             `json:"rotations"`
	KeyIDs      json.RawMessage `json:"key_ids"`
	Failed      int             `json:"failed"`
	FailReasons map[string]int  `json:"fail_reasons"`
}

// orderedKeyIDs renders a shard-index -> key-id map as a JSON object with
// numerically sorted keys.
func orderedKeyIDs(boundaries map[int]string) json.RawMessage {
	idxs := make([]int, 0, len(boundaries))
	for i := range boundaries {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	var buf bytes.Buffer
	buf.WriteByte('{')
	for n, i := range idxs {
		if n > 0 {
			buf.WriteByte(',')
		}
		fmt.Fprintf(&buf, "%s:%s", strconv.Quote(strconv.Itoa(i)), strconv.Quote(boundaries[i]))
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

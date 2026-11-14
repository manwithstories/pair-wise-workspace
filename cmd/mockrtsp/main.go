// Command mockrtsp runs the loopback-only mock RTSP server used to
// demonstrate rtspreplay. All fixtures and fault injection are generated
// in-process; no external files are needed.
//
//	go run ./cmd/mockrtsp -addr 127.0.0.1:8554 -faults drop=4,dup=2,reorder=3
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"rtspreplay/internal/mocksrv"
)

func main() {
	fs := flag.NewFlagSet("mockrtsp", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8554", "listen address (loopback only)")
	path := fs.String("path", "/live", "stream path")
	packets := fs.Int("packets", 500, "RTP packets per session across all tracks")
	rate := fs.Int("rate", 500, "send pace in packets/second")
	faults := fs.String("faults", "", "fault injection: drop=N,dup=N,reorder=N")
	seed := fs.Int64("seed", 1, "deterministic stream seed")
	user := fs.String("user", "user", "digest username (empty disables auth)")
	pass := fs.String("pass", "pass", "digest password")
	_ = fs.Parse(os.Args[1:])

	drop, dup, reorder, err := mocksrv.ParseFaults(*faults)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mockrtsp:", err)
		os.Exit(1)
	}
	srv := mocksrv.New(mocksrv.Config{
		Addr: *addr, Path: *path, Packets: *packets, Rate: *rate,
		Drop: drop, Dup: dup, Reorder: reorder,
		Seed: *seed, User: *user, Pass: *pass,
	})
	if err := srv.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "mockrtsp:", err)
		os.Exit(1)
	}
	fmt.Printf("mockrtsp: serving rtsp://%s%s packets=%d rate=%d faults=%q\n",
		srv.Addr(), *path, *packets, *rate, *faults)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	_ = srv.Close()
}

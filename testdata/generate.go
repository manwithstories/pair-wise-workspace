// 本文件是独立可运行的样例数据生成器（Go 规范会忽略名为 testdata 的目录的
// 可导入性，因此真正的生成器实现位于 internal/fixture，本入口仅做落盘演示）。
//
// 用法:
//
//	go run ./testdata/generate.go --seed 41 --fault "" --out /tmp/mediatime-samples
//
// 生成正常及损坏样例（fMP4 分片与 HLS 清单）到指定目录；不依赖任何外部文件。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"mediatime/internal/fixture"
)

func main() {
	seed := flag.Int64("seed", 41, "deterministic seed")
	fault := flag.String("fault", "", "injected fault")
	out := flag.String("out", "", "output directory for generated samples")
	flag.Parse()

	if *out == "" {
		fmt.Fprintln(os.Stderr, "testdata generator: --out is required")
		os.Exit(2)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "testdata generator: %v\n", err)
		os.Exit(5)
	}

	gen := fixture.NewGenerator(*seed)
	sc, err := gen.Generate(fixture.DefaultFragmentCount, fixture.Fault(*fault))
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdata generator: %v\n", err)
		os.Exit(5)
	}
	for _, spec := range sc.FragmentSpec {
		dir := filepath.Join(*out, filepath.Dir(spec.URI))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "testdata generator: %v\n", err)
			os.Exit(5)
		}
		if err := os.WriteFile(filepath.Join(*out, spec.URI), fixture.BuildFragmentBytes(spec), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "testdata generator: %v\n", err)
			os.Exit(5)
		}
	}
	playlist := sc.HLSPlaylist
	if fixture.Fault(*fault) == fixture.FaultConflict {
		playlist = sc.ConflictPlaylist
	}
	if err := os.WriteFile(filepath.Join(*out, "playlist.m3u8"), []byte(playlist), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "testdata generator: %v\n", err)
		os.Exit(5)
	}
	fmt.Printf("generated %d fragments and playlist.m3u8 into %s\n",
		len(sc.FragmentSpec), *out)
}

// kmsctl 是本地可运行的课件加密分发与授权校验工具。所有子命令纯本地执行，
// 仅依赖 Go 标准库；样例数据由固定种子确定性生成，不引用任何外部文件。
package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kmsctl/internal/audit"
	"kmsctl/internal/cryptox"
	"kmsctl/internal/keydist"
	"kmsctl/internal/keyschedule"
	"kmsctl/internal/license"
	"kmsctl/internal/manifest"
	"kmsctl/internal/replay"
)

// 退出码约定：0 成功；2 参数错误；3 完整性失败；4 许可证时效失败；
// 5 设备绑定/重放失败；6 授权范围/凭据失败。
const (
	exitOK        = 0
	exitUsage     = 2
	exitIntegrity = 3
	exitLicense   = 4
	exitReplay    = 5
	exitAuthz     = 6
)

const defaultMasterKeyHex = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
const fixtureSeed = 10265

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: kmsctl {gen-fixture|encrypt|license|verify|audit} ...")
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "gen-fixture":
		return cmdGenFixture(rest)
	case "encrypt":
		return cmdEncrypt(rest)
	case "license":
		return cmdLicense(rest)
	case "verify":
		return cmdVerify(rest)
	case "audit":
		return cmdAudit(rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", cmd)
		return exitUsage
	}
}

func parseMasterKey(text string) ([]byte, error) {
	key, err := hex.DecodeString(text)
	if err != nil {
		return nil, fmt.Errorf("master-key must be hex: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("master-key must be 32 bytes, got %d", len(key))
	}
	return key, nil
}

func parseTime(text string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return time.Time{}, fmt.Errorf("time must be RFC3339 (e.g. 2026-01-01T00:00:00Z): %w", err)
	}
	return t, nil
}

// reasonOf 解包哨兵错误，返回稳定的失败原因码。
func reasonOf(err error) string {
	for _, target := range []error{
		manifest.ErrTamper, cryptox.ErrAADMismatch, cryptox.ErrTagInvalid,
		license.ErrDeviceMismatch, license.ErrBadSignature, license.ErrExpired,
		license.ErrNotYetValid, keydist.ErrKeyExpired, keydist.ErrOutOfScope,
		keydist.ErrAlreadyUsed, replay.ErrReplay,
	} {
		if errors.Is(err, target) {
			return target.Error()
		}
	}
	return err.Error()
}

func cmdGenFixture(args []string) int {
	fs := flag.NewFlagSet("gen-fixture", flag.ContinueOnError)
	out := fs.String("out", "", "output source file path")
	size := fs.Int("size", 0, "source file size in bytes")
	device := fs.String("device", "", "bound device id")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *out == "" || *size < 0 || *device == "" {
		fmt.Fprintln(os.Stderr, "gen-fixture requires --out, --size>=0 and --device")
		return exitUsage
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}
	rng := rand.New(rand.NewSource(fixtureSeed))
	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}
	defer f.Close()
	buf := make([]byte, 64*1024)
	remaining := *size
	for remaining > 0 {
		n := len(buf)
		if n > remaining {
			n = remaining
		}
		if _, err := io.ReadFull(rng, buf[:n]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitUsage
		}
		if _, err := f.Write(buf[:n]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitUsage
		}
		remaining -= n
	}
	devicePath := strings.TrimSuffix(*out, filepath.Ext(*out)) + ".device.json"
	deviceInfo := map[string]string{"device_id": *device}
	raw := mustJSON(deviceInfo)
	if err := os.WriteFile(devicePath, []byte(raw+"\n"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}
	fmt.Printf(`{"fixture":%q,"size":%d,"device":%q,"device_file":%q}`+"\n",
		*out, *size, *device, devicePath)
	return exitOK
}

func cmdEncrypt(args []string) int {
	fs := flag.NewFlagSet("encrypt", flag.ContinueOnError)
	in := fs.String("in", "", "plaintext input file")
	out := fs.String("out", "", "ciphertext output file")
	manifestPath := fs.String("manifest", "", "manifest output file")
	segmentSize := fs.Int("segment-size", 131072, "segment size in bytes")
	rotateEvery := fs.Int("rotate-every", keyschedule.DefaultRotateEvery, "segments per key rotation")
	epochSeconds := fs.Int64("epoch-seconds", 0, "wall-clock rotation interval in virtual seconds (0 disables)")
	masterKeyHex := fs.String("master-key", defaultMasterKeyHex, "hex-encoded 32-byte master key")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *in == "" || *out == "" || *manifestPath == "" || *segmentSize <= 0 {
		fmt.Fprintln(os.Stderr, "encrypt requires --in, --out, --manifest and --segment-size>0")
		return exitUsage
	}
	masterKey, err := parseMasterKey(*masterKeyHex)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}
	schedule, err := keyschedule.New(masterKey, *rotateEvery, *epochSeconds)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}
	inFile, err := os.Open(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}
	defer inFile.Close()
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}
	outFile, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}
	defer outFile.Close()

	info, err := inFile.Stat()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}
	segmentCount := int((info.Size() + int64(*segmentSize) - 1) / int64(*segmentSize))
	m := &manifest.Manifest{Header: manifest.Header{
		Version:      1,
		Algorithm:    "AES-256-GCM",
		SegmentSize:  *segmentSize,
		RotateEvery:  *rotateEvery,
		EpochSeconds: *epochSeconds,
		SegmentCount: segmentCount,
		CipherFile:   filepath.Base(*out),
		MasterKeyID:  manifest.MasterKeyID(masterKey),
	}}
	// 摘要在段数确定后计算，保证加密侧与校验侧 AAD 绑定的清单摘要完全一致。
	digest := m.Digest()
	buf := make([]byte, *segmentSize)
	var offset int64
	for index := 0; ; index++ {
		n, readErr := io.ReadFull(inFile, buf)
		if readErr == io.EOF {
			break
		}
		if readErr != nil && readErr != io.ErrUnexpectedEOF {
			fmt.Fprintln(os.Stderr, readErr)
			return exitUsage
		}
		plain := buf[:n]
		epoch := schedule.EpochFor(int64(index))
		keyID := schedule.KeyID(int64(index))
		nonce := cryptox.DeriveNonce(epoch, int64(index))
		aad := cryptox.BuildAAD(int64(index), keyID, digest)
		sealed, err := cryptox.Seal(schedule.KeyFor(int64(index)), nonce, plain, aad)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitUsage
		}
		if _, err := outFile.Write(sealed); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitUsage
		}
		m.Segments = append(m.Segments, manifest.SegmentRecord{
			Index:        index,
			Offset:       offset,
			CipherLength: len(sealed),
			PlainLength:  n,
			KeyID:        keyID,
			Epoch:        epoch,
			Nonce:        hex.EncodeToString(nonce),
			Tag:          hex.EncodeToString(sealed[len(sealed)-16:]),
		})
		offset += int64(len(sealed))
		if readErr == io.ErrUnexpectedEOF {
			break
		}
	}
	if len(m.Segments) != segmentCount {
		fmt.Fprintf(os.Stderr, "internal error: expected %d segments, wrote %d\n", segmentCount, len(m.Segments))
		return exitUsage
	}
	if err := m.Save(*manifestPath, masterKey); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}
	fmt.Printf(`{"segment_count":%d,"rotation_count":%d,"manifest":%q}`+"\n",
		len(m.Segments), schedule.RotationCount(len(m.Segments)), *manifestPath)
	return exitOK
}

func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func cmdLicense(args []string) int {
	if len(args) == 0 || args[0] != "issue" {
		fmt.Fprintln(os.Stderr, "usage: kmsctl license issue ...")
		return exitUsage
	}
	fs := flag.NewFlagSet("license issue", flag.ContinueOnError)
	device := fs.String("device", "", "bound device id")
	segRange := fs.String("segments", "", "closed segment range, e.g. 0-15")
	notBeforeText := fs.String("not-before", "", "RFC3339 not-before")
	notAfterText := fs.String("not-after", "", "RFC3339 not-after")
	out := fs.String("out", "", "license output path")
	masterKeyHex := fs.String("master-key", defaultMasterKeyHex, "hex 32-byte master key")
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsage
	}
	first, last, err := license.ParseRange(*segRange)
	if err != nil || *device == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "license issue requires --device, --segments, --out")
		return exitUsage
	}
	notBefore, err := parseTime(*notBeforeText)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}
	notAfter, err := parseTime(*notAfterText)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}
	masterKey, err := parseMasterKey(*masterKeyHex)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}
	lic, err := license.Issue(masterKey, license.Claim{
		Device:    *device,
		FirstSeg:  first,
		LastSeg:   last,
		NotBefore: notBefore,
		NotAfter:  notAfter,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}
	if err := lic.Save(*out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}
	fmt.Printf(`{"device":%q,"segments":%q,"out":%q}`+"\n", *device, *segRange, *out)
	return exitOK
}

func cmdAudit(args []string) int {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	reportPath := fs.String("report", "", "report json produced by verify")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *reportPath == "" {
		fmt.Fprintln(os.Stderr, "audit requires --report")
		return exitUsage
	}
	report, err := audit.Load(*reportPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}
	fmt.Println(report.JSON())
	return exitOK
}

// exitForReason 把失败原因码映射为约定退出码。
func exitForReason(reason string) int {
	switch reason {
	case manifest.ErrTamper.Error(), cryptox.ErrAADMismatch.Error(), cryptox.ErrTagInvalid.Error():
		return exitIntegrity
	case license.ErrExpired.Error(), license.ErrNotYetValid.Error():
		return exitLicense
	case license.ErrDeviceMismatch.Error(), license.ErrBadSignature.Error(), replay.ErrReplay.Error():
		return exitReplay
	case keydist.ErrKeyExpired.Error(), keydist.ErrOutOfScope.Error(), keydist.ErrAlreadyUsed.Error():
		return exitAuthz
	default:
		return exitIntegrity
	}
}

func writeFailureReport(path string, rotationCount int, reason string) (int, error) {
	report := audit.Build(rotationCount, nil, []audit.Failure{{Index: -1, Reason: reason}})
	if path != "" {
		return exitForReason(reason), report.Save(path)
	}
	fmt.Println(report.JSON())
	return exitForReason(reason), nil
}

func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	manifestPath := fs.String("manifest", "", "manifest json")
	licensePath := fs.String("license", "", "license json")
	device := fs.String("device", "", "presenting device id")
	nowText := fs.String("now", "", "RFC3339 verification time")
	reportPath := fs.String("report", "", "report output path")
	cipherPath := fs.String("ciphertext", "", "ciphertext file (defaults next to manifest)")
	ledgerPath := fs.String("ledger", "out/replay.ledger.json", "replay ledger path")
	masterKeyHex := fs.String("master-key", defaultMasterKeyHex, "hex 32-byte master key")
	skew := fs.Int64("max-skew", license.DefaultClockSkew, "tolerated clock skew in seconds")
	credTTL := fs.Duration("cred-ttl", 5*time.Minute, "one-time credential TTL")
	continueOnError := fs.Bool("continue-on-error", false, "record every segment failure instead of stopping")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *manifestPath == "" || *licensePath == "" || *device == "" || *nowText == "" {
		fmt.Fprintln(os.Stderr, "verify requires --manifest, --license, --device and --now")
		return exitUsage
	}
	now, err := parseTime(*nowText)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}
	masterKey, err := parseMasterKey(*masterKeyHex)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}
	m, err := manifest.Load(*manifestPath, masterKey)
	if err != nil {
		code, saveErr := writeFailureReport(*reportPath, 0, reasonOf(err))
		if saveErr != nil {
			fmt.Fprintln(os.Stderr, saveErr)
		}
		return code
	}
	lic, err := license.Load(*licensePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}
	if err := lic.VerifyAt(masterKey, *device, now, *skew); err != nil {
		code, saveErr := writeFailureReport(*reportPath, 0, reasonOf(err))
		if saveErr != nil {
			fmt.Fprintln(os.Stderr, saveErr)
		}
		return code
	}
	if *cipherPath == "" {
		*cipherPath = filepath.Join(filepath.Dir(*manifestPath), m.Header.CipherFile)
	}
	cipherFile, err := os.Open(*cipherPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}
	defer cipherFile.Close()

	schedule, err := keyschedule.New(masterKey, m.Header.RotateEvery, m.Header.EpochSeconds)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}
	clock := now
	distributor := keydist.New(schedule, *credTTL, func() time.Time { return clock })
	cred := distributor.Issue(lic)
	ledger, err := replay.Open(*ledgerPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}
	digest := m.Digest()
	seenNonce := map[string]bool{}
	statuses := make([]audit.SegmentStatus, 0, len(m.Segments))
	failures := []audit.Failure{}
	firstReason := ""
	recordFailure := func(index int, reason string) int {
		failures = append(failures, audit.Failure{Index: index, Reason: reason})
		if firstReason == "" {
			firstReason = reason
		}
		return exitForReason(reason)
	}
	for _, rec := range m.Segments {
		reason := ""
		if clock.Before(lic.Claim.NotBefore) || clock.After(lic.Claim.NotAfter) {
			if clock.After(lic.Claim.NotAfter) {
				reason = license.ErrExpired.Error()
			} else {
				reason = license.ErrNotYetValid.Error()
			}
		} else if !lic.Covers(rec.Index) {
			reason = keydist.ErrOutOfScope.Error()
		} else {
			key, _, redeemErr := distributor.Redeem(cred, rec.Index)
			if redeemErr != nil {
				reason = reasonOf(redeemErr)
			} else {
				var checkErr error
				if seenNonce[cred.Nonce] {
					checkErr = ledger.Check(cred.Device, cred.Sequence)
				} else {
					checkErr = ledger.CheckNew(cred.Device, cred.Sequence, cred.Nonce)
				}
				if checkErr != nil {
					reason = reasonOf(checkErr)
				}
			}
			if reason == "" {
				nonce, decodeErr := hex.DecodeString(rec.Nonce)
				if decodeErr != nil {
					reason = cryptox.ErrAADMismatch.Error()
				} else {
					sealed := make([]byte, rec.CipherLength)
					if _, readErr := cipherFile.ReadAt(sealed, rec.Offset); readErr != nil {
						fmt.Fprintln(os.Stderr, readErr)
						return exitUsage
					}
					aad := cryptox.BuildAAD(int64(rec.Index), rec.KeyID, digest)
					if _, openErr := cryptox.Open(key, nonce, sealed, aad, int64(rec.Index), rec.KeyID, digest); openErr != nil {
						reason = reasonOf(openErr)
					} else if commitErr := ledger.Commit(cred.Device, cred.Sequence, cred.Nonce); commitErr != nil {
						fmt.Fprintln(os.Stderr, commitErr)
						return exitUsage
					} else {
						seenNonce[cred.Nonce] = true
					}
				}
			}
		}
		status := "ok"
		if reason != "" {
			status = "failed"
			code := recordFailure(rec.Index, reason)
			statuses = append(statuses, audit.SegmentStatus{
				Index: rec.Index, KeyID: rec.KeyID, Epoch: rec.Epoch, Status: status, Reason: reason,
			})
			if !*continueOnError {
				report := audit.Build(schedule.RotationCount(len(m.Segments)), statuses, failures)
				if *reportPath != "" {
					_ = report.Save(*reportPath)
				} else {
					fmt.Println(report.JSON())
				}
				return code
			}
		} else {
			statuses = append(statuses, audit.SegmentStatus{
				Index: rec.Index, KeyID: rec.KeyID, Epoch: rec.Epoch, Status: status,
			})
		}
	}
	report := audit.Build(schedule.RotationCount(len(m.Segments)), statuses, failures)
	if *reportPath != "" {
		if err := report.Save(*reportPath); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitUsage
		}
	}
	fmt.Println(report.JSON())
	if firstReason != "" {
		return exitForReason(firstReason)
	}
	return exitOK
}

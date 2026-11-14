// Package replay 按设备维护单调递增的凭据序号与已使用 nonce 账本，并原子
// 落盘。进程重启后账本仍然有效，从而识别跨进程的重放与序号倒退。
package replay

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrReplay 表示同一设备重复使用 nonce 或序号倒退/重复。
var ErrReplay = errors.New("REPLAY")

// DeviceState 是单台设备的账本状态。
type DeviceState struct {
	LastSequence uint64   `json:"last_sequence"`
	UsedNonces   []string `json:"used_nonces"`
}

// Ledger 是落盘的全部设备账本。
type Ledger struct {
	path    string                  `json:"-"`
	Devices map[string]*DeviceState `json:"devices"`
}

// Open 读取账本；文件不存在时初始化空账本（首次运行）。
func Open(path string) (*Ledger, error) {
	l := &Ledger{path: path, Devices: map[string]*DeviceState{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return l, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, l); err != nil {
		return nil, fmt.Errorf("corrupt replay ledger: %w", err)
	}
	if l.Devices == nil {
		l.Devices = map[string]*DeviceState{}
	}
	return l, nil
}

// Check 检查序号是否倒退。同一一次性凭据会被多个段复用，因此相等合法，
// 仅当序号小于该设备已记录的最大序号时判定为序号倒退重放。
func (l *Ledger) Check(device string, sequence uint64) error {
	state := l.Devices[device]
	if state == nil {
		return nil
	}
	if sequence < state.LastSequence {
		return fmt.Errorf("%w: device %s sequence %d < last %d",
			ErrReplay, device, sequence, state.LastSequence)
	}
	return nil
}

// CheckNew 在凭据首次出现时调用：除序号倒退外，还检查 nonce 是否在历史
// 账本中出现过（跨进程重放）。
func (l *Ledger) CheckNew(device string, sequence uint64, nonce string) error {
	if err := l.Check(device, sequence); err != nil {
		return err
	}
	state := l.Devices[device]
	if state == nil {
		return nil
	}
	for _, used := range state.UsedNonces {
		if used == nonce {
			return fmt.Errorf("%w: device %s reused nonce", ErrReplay, device)
		}
	}
	return nil
}

// Commit 记录本次成功使用的序号与 nonce，并原子写回磁盘。
func (l *Ledger) Commit(device string, sequence uint64, nonce string) error {
	state := l.Devices[device]
	if state == nil {
		state = &DeviceState{}
		l.Devices[device] = state
	}
	state.LastSequence = sequence
	state.UsedNonces = append(state.UsedNonces, nonce)
	return l.persist()
}

// persist 先写临时文件再 rename，保证账本不会因崩溃写坏。
func (l *Ledger) persist() error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// Package job 实现任务状态机、检查点日志与并发调度。
package job

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// State 是任务状态机的状态。
type State string

const (
	StatePending   State = "pending"
	StatePlanning  State = "planning"
	StateEncoding  State = "encoding"
	StateVerifying State = "verifying"
	StateDone      State = "done"
	StateFailed    State = "failed"
)

// transitions 定义合法迁移。
var transitions = map[State][]State{
	StatePending:   {StatePlanning, StateFailed},
	StatePlanning:  {StateEncoding, StateFailed},
	StateEncoding:  {StateVerifying, StateFailed},
	StateVerifying: {StateDone, StateFailed},
	StateFailed:    {StatePending}, // 允许续跑时重置
}

// Event 是一条状态机事件日志。
type Event struct {
	Time time.Time `json:"time"`
	Kind string    `json:"kind"` // transition | illegal_transition
	From State     `json:"from"`
	To   State     `json:"to"`
	Note string    `json:"note,omitempty"`
}

// Machine 是带事件日志的任务状态机。
type Machine struct {
	mu      sync.Mutex
	state   State
	logPath string
}

// NewMachine 创建状态机，事件追加写入 logPath。
func NewMachine(logPath string, initial State) *Machine {
	return &Machine{state: initial, logPath: logPath}
}

// State 返回当前状态。
func (m *Machine) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Transition 尝试迁移；非法迁移记录事件并返回错误。
func (m *Machine) Transition(to State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, allowed := range transitions[m.state] {
		if allowed == to {
			from := m.state
			m.state = to
			m.appendLocked(Event{Time: time.Now(), Kind: "transition", From: from, To: to})
			return nil
		}
	}
	ev := Event{Time: time.Now(), Kind: "illegal_transition", From: m.state, To: to,
		Note: fmt.Sprintf("不允许 %s -> %s", m.state, to)}
	m.appendLocked(ev)
	return fmt.Errorf("非法状态迁移 %s -> %s", m.state, to)
}

func (m *Machine) appendLocked(ev Event) {
	if m.logPath == "" {
		return
	}
	f, err := os.OpenFile(m.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(ev)
	f.Write(append(b, '\n'))
}

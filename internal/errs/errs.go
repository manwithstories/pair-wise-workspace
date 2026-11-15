// Package errs defines coded errors shared across all modules.
package errs

import "fmt"

// Exit codes used by the dashpack CLI.
const (
	ExitOK        = 0
	ExitUsage     = 2 // 参数或输入解析失败
	ExitAlignment = 3 // 对齐或校验不通过
	ExitWrite     = 4 // 写入失败
)

// Error codes.
const (
	EDuration = "E_DURATION"
	EManifest = "E_MANIFEST"
	ETemplate = "E_TEMPLATE"
	ESource   = "E_SOURCE"
	EEmpty    = "E_EMPTY"
	EWrite    = "E_WRITE"
	EAlign    = "E_ALIGN"
	EVerify   = "E_VERIFY"
)

// Coded is an error carrying a machine-readable code and a process exit code.
type Coded struct {
	Code string
	Exit int
	Msg  string
}

func (e *Coded) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Msg) }

// New builds a Coded error.
func New(code string, exit int, format string, args ...any) *Coded {
	return &Coded{Code: code, Exit: exit, Msg: fmt.Sprintf(format, args...)}
}

// ExitCode maps an error to a process exit code.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	if c, ok := err.(*Coded); ok {
		return c.Exit
	}
	return ExitUsage
}

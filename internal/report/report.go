// Package report 把 validate.Report 渲染为稳定排序的 JSON 与终端摘要。
// 输出只包含输入文件本身的信息与相对文件名，绝不读取环境变量或外部路径。
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"hdrcheck/internal/hdr"
	"hdrcheck/internal/validate"
)

// Envelope 是 JSON 输出的顶层结构，键顺序固定，数组全部稳定排序。
type Envelope struct {
	Schema       string                 `json:"schema"`
	Input        InputMeta              `json:"input"`
	Summary      validate.Summary       `json:"summary"`
	Tracks       []validate.TrackReport `json:"tracks"`
	GlobalIssues []hdr.Issue            `json:"global_issues"`
}

// InputMeta 是输入文件的安全摘要（仅文件名，不含目录，避免路径泄露）。
type InputMeta struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

// BuildEnvelope 把内部报告转为可序列化外壳，并对全部问题做稳定排序。
func BuildEnvelope(inputName string, rep *validate.Report) Envelope {
	tracks := make([]validate.TrackReport, len(rep.Tracks))
	copy(tracks, rep.Tracks)
	sort.SliceStable(tracks, func(i, j int) bool { return tracks[i].TrackID < tracks[j].TrackID })
	for i := range tracks {
		sortIssues(tracks[i].Issues)
	}
	global := append([]hdr.Issue{}, rep.GlobalIssues...)
	sortIssues(global)
	return Envelope{
		Schema: "hdrcheck-report/v1",
		Input: InputMeta{
			Name:      safeBaseName(inputName),
			SizeBytes: rep.Summary.InputBytes,
			SHA256:    rep.Summary.InputSHA256,
		},
		Summary:      rep.Summary,
		Tracks:       tracks,
		GlobalIssues: global,
	}
}

// WriteJSON 输出缩进、稳定排序的 JSON。
func WriteJSON(w io.Writer, env Envelope) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(env); err != nil {
		return fmt.Errorf("report: write json: %w", err)
	}
	return nil
}

// WriteText 输出人类可读终端摘要：来源、最大内容亮度、问题等级、数量、哈希。
func WriteText(w io.Writer, env Envelope) error {
	var b strings.Builder
	fmt.Fprintf(&b, "hdrcheck report — %s\n", env.Input.Name)
	fmt.Fprintf(&b, "  input_sha256            : %s\n", env.Input.SHA256)
	fmt.Fprintf(&b, "  input_bytes             : %d\n", env.Input.SizeBytes)
	fmt.Fprintf(&b, "  video_track_count       : %d\n", env.Summary.VideoTrackCount)
	fmt.Fprintf(&b, "  conflict_count          : %d\n", env.Summary.ConflictCount)
	fmt.Fprintf(&b, "  missing_count           : %d\n", env.Summary.MissingCount)
	fmt.Fprintf(&b, "  error_count             : %d\n", env.Summary.ErrorCount)
	fmt.Fprintf(&b, "  warning_count           : %d\n", env.Summary.WarningCount)
	b.WriteString("  per-track summary:\n")
	for _, tr := range env.Tracks {
		fmt.Fprintf(&b, "    - track %d hdr_flag=%t\n", tr.TrackID, tr.HDRFlag)
		fmt.Fprintf(&b, "        mastering_display   : present=%-5v source=%-9s status=%s\n",
			tr.Mastering.Present, tr.Mastering.Source, tr.Mastering.Status)
		fmt.Fprintf(&b, "        max_content_light   : present=%-5v source=%-9s status=%s",
			tr.MaxCLL.Present, tr.MaxCLL.Source, tr.MaxCLL.Status)
		if tr.MaxCLL.Present {
			fmt.Fprintf(&b, " nits=%d", tr.MaxCLL.Nits)
		}
		b.WriteByte('\n')
		fmt.Fprintf(&b, "        max_frame_avg_light: present=%-5v source=%-9s status=%s",
			tr.MaxFALL.Present, tr.MaxFALL.Source, tr.MaxFALL.Status)
		if tr.MaxFALL.Present {
			fmt.Fprintf(&b, " nits=%d", tr.MaxFALL.Nits)
		}
		b.WriteByte('\n')
		writeIssueList(&b, "track", tr.Issues)
	}
	writeIssueList(&b, "global", env.GlobalIssues)
	if env.Summary.ErrorCount+env.Summary.WarningCount == 0 {
		b.WriteString("  no inconsistencies detected\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func writeIssueList(b *strings.Builder, scope string, list []hdr.Issue) {
	if len(list) == 0 {
		return
	}
	fmt.Fprintf(b, "      %s issues:\n", scope)
	for _, is := range list {
		loc := ""
		if is.TrackID != 0 {
			loc = fmt.Sprintf(" track=%d", is.TrackID)
		}
		fmt.Fprintf(b, "        [%s] %s%s @offset=%d — %s\n",
			is.Severity, is.Code, loc, is.Offset, is.Message)
		if is.Hint != "" {
			fmt.Fprintf(b, "          hint: %s\n", is.Hint)
		}
	}
}

func sortIssues(list []hdr.Issue) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].TrackID != list[j].TrackID {
			return list[i].TrackID < list[j].TrackID
		}
		if list[i].Offset != list[j].Offset {
			return list[i].Offset < list[j].Offset
		}
		if list[i].Code != list[j].Code {
			return list[i].Code < list[j].Code
		}
		return list[i].Message < list[j].Message
	})
}

// safeBaseName 只保留文件名部分，防止目录结构经输出外泄。
func safeBaseName(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

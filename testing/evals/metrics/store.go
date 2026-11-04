package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// 落盘文件名：manifest 与 summary 分开存。
// 为什么要单独存 JSON：报告是给人读的，基线对比与历史趋势要机器读；
// 只有 report.md 的话，下一次 run 没法可靠地拿回上次的数字（解析 Markdown 等于自找麻烦）。
const (
	ManifestFile = "manifest.json"
	SummaryFile  = "summary.json"
)

// SaveJSON 把快照与总览写到 dir（两次写文件，任一失败即报错，不吞）。
func SaveJSON(dir string, s Summary) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, ManifestFile), s.Manifest); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, SummaryFile), s)
}

// LoadSummary 从 dir 或 summary.json 路径读回一份总览（基线对比用）。
func LoadSummary(path string) (Summary, error) {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		path = filepath.Join(path, SummaryFile)
	}
	var s Summary
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, err
	}
	return s, nil
}

// SaveBaseline 把总览写成一个基线文件（默认 <out>/baseline.json）。
// 与 SaveJSON 分开：run 目录里那份是这次 run 的证据，基线是"上一次公认的好成绩"。
func SaveBaseline(path string, s Summary) error {
	return writeJSON(path, s)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

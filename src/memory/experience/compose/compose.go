// Package compose 是经验域私有渲染：把一条长期记忆渲染成注入行。
package compose

// EntryLine 是一条记忆条目的展示行（核心 / 相关节共用）。
func EntryLine(kind, content string) string {
	return "- [" + kind + "] " + content
}

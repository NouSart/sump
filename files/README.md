# files 服务

SUMP v2 的文件库：独立 SQLite 元数据 + 二进制目录，支持保存、取回、读取文本、就地编辑。

## 动作

| 动作 | 输入 | 输出 | 说明 |
|------|------|------|------|
| `save` | `{url}` 或 `{data(base64), filename, mime}` | `{id, filename, mime, size}` | 保存文件，返回 id |
| `fetch` | `{id}` | `{id, filename, mime, data(base64), size}` | 取回完整文件 |
| `read` | `{id}` | `{id, filename, text, truncated}` | LLM 工具；支持文本类、`.xlsx` 转 TSV、`.docx`/`.pptx` 提取正文；二进制报错 |
| `write` | `{id, sheet, edits:[{cell, value}]}` | `{id, sheet, updated}` | LLM 工具；仅编辑 `.xlsx` 单元格 |
| `edit_word` | `{id, operations:[{op, old, new}]}` | `{id, replaced}` | LLM 工具；仅编辑 `.docx` 正文；v0 仅 `op="replace"` |
| `edit_ppt` | `{id, operations:[{op, old, new}]}` | `{id, replaced}` | LLM 工具；仅编辑 `.pptx` 幻灯片文本；v0 仅 `op="replace"` |

## 限制

- 单文件上限默认 100MiB，可通过 `-max-mb` 或设置中心 `max_mb` 调整。
- `read` 返回文本上限默认 4000 字符，可通过 `-read-max-chars` 或设置中心 `read_max_chars` 调整。
- 旧版 `.xls` 与 `.doc`（OLE2）不支持阅读和编辑，会提示另存为 `.xlsx` / `.docx`。
- `edit_word` v0 为纯文本占位符替换：要求 `old` 在 `word/document.xml` 的某个 `<w:t>` 节点内连续出现；不支持跨 run、段落/表格结构增删、图片替换。未知 `op` 类型直接报错。
- `edit_ppt` v0 为纯文本占位符替换：在所有 `ppt/slides/slideN.xml` 中替换 `old`；要求 `old` 在单个 `<a:t>` 节点内连续出现；不改动母版、版式、备注、图表数据；不支持新增/删除幻灯片。未知 `op` 类型直接报错。

## 运行

```powershell
cd files
go run .
```

## 测试

```powershell
cd files
go test ./... -count=1
```

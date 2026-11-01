# files 服务

> **读者**：要从链上存取文件、或改文件读写能力的人 ｜ **读完能做**：按 id 存取、读懂截断与拒绝规则 ｜ **最后校验**：2026-10-30

SUMP v2 的文件库：独立 SQLite 元数据 + 二进制目录，支持保存、取回、读取文本、就地编辑。

## 动作

| 动作 | 输入 | 输出 | 说明 |
|------|------|------|------|
| `save` | `{url}` 或 `{data(base64), filename, mime}` | `{id, filename, mime, size}` | 保存文件，返回 id |
| `fetch` | `{id}` | `{id, filename, mime, data(base64), size}` | 取回完整文件 |
| `read` | `{id}` | `{id, filename, text, truncated}` | LLM 工具；支持文本类、`.xlsx` 转 TSV、`.docx`/`.pptx`/`.pdf` 提取正文；二进制报错 |
| `write` | `{id, sheet, edits:[{cell, value}]}` | `{id, sheet, updated}` | LLM 工具；仅编辑 `.xlsx` 单元格 |
| `edit_word` | `{id, operations:[{op, old, new}]}` | `{id, replaced}` | LLM 工具；仅编辑 `.docx` 正文；v0 仅 `op="replace"` |
| `edit_ppt` | `{id, operations:[{op, old, new}]}` | `{id, replaced}` | LLM 工具；仅编辑 `.pptx` 幻灯片文本；v0 仅 `op="replace"` |
| `edit_pdf` | `{id, operations:[{op, pages?, degrees?, properties?}]}` | `{id, applied}` | LLM 工具；仅编辑 `.pdf`；v0 支持 `set_info`/`rotate`/`delete_pages`/`keep_pages` |
| `outline` | `{id, depth?}` | `{id, doc_kind, digest, nodes, node_count, chars, tables, parts, warnings}` | LLM 工具；文档结构树（段落 / 表格逐层展开）+ 整篇指纹 `digest` |
| `read_range` | `{id, node?, offset?, limit?}` | `{id, doc_kind, digest, node, text, total, next}` | LLM 工具；按节点分页读，`node` 空 = 整篇，`next` 非 0 表示还有下一块 |
| `find` | `{id, text, node?, regex?, limit?}` | `{id, digest, matches:[{node, run, start, end, context}]}` | LLM 工具；只回命中位置（节点地址 + rune 区间 + 上下文），不改文档 |
| `patch` | `{id, anchor?, operations:[{op, node, range?, text?, anchor?}]}` | `{id, applied, digest, diff, warnings}` | LLM 工具；按节点地址就地改 `.docx`；`op` = `replace_text`/`delete_node`，整批原子 |
| `verify` | `{id, digest?, contains?, not_contains?}` | `{id, ok, digest, failures}` | LLM 工具；改完自查（指纹 + 片段核对），不通过也只是回事实不报错 |

## 限制

- 单文件上限默认 100MiB，可通过 `-max-mb` 或设置中心 `max_mb` 调整。
- `read` 返回文本上限默认 4000 字符，可通过 `-read-max-chars` 或设置中心 `read_max_chars` 调整。
- 旧版 `.xls` 与 `.doc`（OLE2）不支持阅读和编辑，会提示另存为 `.xlsx` / `.docx`。
- `edit_word` v0 为纯文本占位符替换：要求 `old` 在 `word/document.xml` 的某个 `<w:t>` 节点内连续出现；不支持跨 run、段落/表格结构增删、图片替换。未知 `op` 类型直接报错。
- `edit_ppt` v0 为纯文本占位符替换：在所有 `ppt/slides/slideN.xml` 中替换 `old`；要求 `old` 在单个 `<a:t>` 节点内连续出现；**`new` 不能含换行或制表符**（pptx 的换行是段落级 `<a:br/>`，改 run 结构不在 v0 范围内，硬塞会写出需要修复的文件）；不改动母版、版式、备注、图表数据；不支持新增/删除幻灯片。未知 `op` 类型直接报错。
- `read` / `edit_ppt` 的幻灯片顺序以 `ppt/presentation.xml` 的 `<p:sldIdLst>` 为准（与 PowerPoint 的放映顺序一致），不按 `slideN.xml` 的文件名编号；缺 `presentation.xml` 或其 rels、或清单损坏解析失败时整体退回文件名顺序。
- `edit_pdf` v0 仅做元数据与页操作：`set_info` 写 Info 字典（键用标准名 `Title`/`Author`/`Subject`/`Keywords` 等）、`rotate` 按 90 的整数倍旋转、`delete_pages` 删页、`keep_pages` 只保留指定页。**不支持任意文本替换**（字体子集/编码使纯 Go 可靠替换不可行）、不新增/重排页面结构、不做水印/合并/加解密（这些留给后续 op 扩展）。未知 `op` 类型直接报错。
- PDF 读取按页提取纯文本，页数受上限封顶；加密 PDF 明确拒绝；扫描件（图片型，无文本层）读取报错引导，不做 OCR。
- `outline`/`read_range`/`find`/`patch`/`verify` v0 只接 `.docx`（其它格式会明确报"仅支持按节点编辑 .docx"）。节点地址形如 `body/p3`、`body/tbl1/r2/c1/p1`；写到 run 一级（`body/p3/r2`）会归到所在段落。文本框内的段落是 `body/p3/p1`、内容控件里的段落是 `body/sdt1/p1`——它们和正文段落走同一份投影，`read` 看得见的文字都有节点地址。
- `patch` 的 `replace_text` 只在受影响 run 内改字（第一处写新文本、后续只删旧字），**不新建 run**，因此不会丢字符样式；`delete_node` 只清空段落文字、保留 `<w:p>`（样式 / 编号还在）。换行写成 `<w:br/>`、制表写成 `<w:tab/>`，不落裸控制字符。
- `patch` 是整批原子的：任意一条失败（节点不存在、区间越界、`anchor` 对不上整篇指纹、未知 `op`）就整批拒绝，库里的字节不变；全部成功才回写一次。
- `verify` 的 `digest` 是整篇可见文本的规范化指纹，可直接拿 `outline`/`find`/`patch` 返回值当"版本号"做乐观锁。

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

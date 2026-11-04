// Package docmodel 是文件服务的文档模型层：把"文档"抽象成有 ID 的节点、
// 可精确定位的编辑操作与可回放的差异，供各格式（docx / pptx / xlsx / pdf）实现。
//
// 为什么要有这一层：v0 的编辑是整篇字符串替换——old 必须恰好落在一个 <w:t> 里
// （Word 常把一句话拆成多个 run），且命中几处就改几处。当需求变成"改对那一处、
// 并且能证明只改了那一处"时，字符串替换给不出答案。所以这里把四件事分开：
//
//	Outline   文档长什么样（先看目录，而不是先吃 4000 字截断）
//	Find      哪些节点命中（只回候选 ID，不直接改）
//	ReadRange 按节点分页读（绕开 read 的字符上限）
//	Patch     按节点 ID 改（Anchor 校验版本漂移，Diff 说明改了什么）
//
// 本包只有类型与注册表，不含任何格式实现——格式细节留在各自子包里，
// 服务层（files/server）只认这里的类型：解析入参、存储、错误映射。
package docmodel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"sync"
)

// Kind 是文档类型，与协议载荷的 doc_kind 同名。
type Kind string

const (
	KindDocx Kind = "docx"
	KindPptx Kind = "pptx"
	KindXlsx Kind = "xlsx"
	KindPDF  Kind = "pdf"
	KindText Kind = "text"
)

// String 返回类型名。
func (k Kind) String() string { return string(k) }

// NodeType 是节点种类：模型据此决定能对它做什么（段落能改字，表格能改单元格）。
type NodeType string

const (
	NodeDocument  NodeType = "document"  // 整篇（root）
	NodeParagraph NodeType = "paragraph" // 段落（docx 的 w:p、pptx 的 a:p）
	NodeRun       NodeType = "run"       // 段落内的一段同格式文字
	NodeTable     NodeType = "table"     // 表格
	NodeRow       NodeType = "row"       // 表格行
	NodeCell      NodeType = "cell"      // 单元格
	NodeSlide     NodeType = "slide"     // 幻灯片
	NodeShape     NodeType = "shape"     // 幻灯片上的形状/文本框/图片
	NodeSheet     NodeType = "sheet"     // 工作表
	NodePage      NodeType = "page"      // PDF 页
)

// NodeID 是节点的稳定地址。约定（各格式只在末段变化）：
//
//	docx  body/p12            正文第 12 个段落
//	      body/tbl3/r2/c1     第 3 个表格第 2 行第 1 列
//	      p12/r3              第 12 段第 3 个 run
//	      body/p12/p1         第 12 段文本框里的第 1 个嵌套段落
//	      body/sdt1/p2        第 1 个内容控件里的第 2 个段落
//	pptx  slide4/sp2          第 4 张幻灯片上 shape id=2 的形状
//	xlsx  sheet1!C7           工作表 sheet1 的 C7 单元格
//	pdf   page2               第 2 页
//
// ID 的稳定性来自"顺序"或"文件自带的 id"（pptx 的 cNvPr/@id、xlsx 的坐标、
// pdf 的页码），不依赖文本内容——文本会变，地址不能跟着变。
type NodeID string

// String 返回 ID 文本。
func (id NodeID) String() string { return string(id) }

// Valid 只做最低限度的形状校验：非空且不含空白（避免把模型幻觉出来的
// "第 3 段" 之类当地址用）。真正的解析在各格式实现里。
func (id NodeID) Valid() bool {
	if id == "" {
		return false
	}
	return !strings.ContainsAny(string(id), " \t\r\n")
}

// Node 是文档结构树上的一个节点。
type Node struct {
	ID   NodeID   `json:"id"`
	Type NodeType `json:"type"`
	Text string   `json:"text,omitempty"` // 文本预览（已截断，用于让模型决定读哪段）
	// Level 是标题层级（1-9，0 = 不是标题），来自 docx 的 pStyle / pptx 的标题占位符。
	Level    int    `json:"level,omitempty"`
	Children []Node `json:"children,omitempty"`
	// Truncated 表示 Text 只是预览（ReadRange 能取全）。
	Truncated bool `json:"truncated,omitempty"`
	// ExactAnchor 是节点原始文本的指纹（段落节点才有）；range 编辑用它校验坐标。
	ExactAnchor string `json:"exact_anchor,omitempty"`
}

// Stats 是文档规模统计：用于分诊（大文件先 outline）与预算判断。
type Stats struct {
	Nodes  int `json:"nodes"`            // 树上的节点总数
	Chars  int `json:"chars"`            // 可见字符总数（rune）
	Tables int `json:"tables,omitempty"` // 表格数
	Pages  int `json:"pages,omitempty"`  // 页数（pdf / pptx 有天然页）
	Parts  int `json:"parts,omitempty"`  // OPC part 数（docx/pptx/xlsx）
}

// Outline 是文档的目录（结构树 + 规模）。
type Outline struct {
	Kind  Kind   `json:"kind"`
	Nodes []Node `json:"nodes"`
	Stats Stats  `json:"stats"`
	// Warnings 是"读得懂但需要提醒"的事：修订未接受、外链图片、宏被剥离等。
	Warnings []string `json:"warnings,omitempty"`
}

// Query 是一次查找：只回候选，不改文档。
type Query struct {
	Text  string `json:"text"`            // 要找的文本
	Node  NodeID `json:"node,omitempty"`  // 限定在某个节点内（空 = 全文）
	Regex bool   `json:"regex,omitempty"` // Text 按正则解释
	Limit int    `json:"limit,omitempty"` // 最多回几条（0 = 默认上限）
}

// Match 是一处命中：定位到节点 + 节点文本投影上的字符区间。
// Node 是编辑要用的地址；Start/End 是 rune 偏移（不是字节，模型读得懂）。
type Match struct {
	Node        NodeID `json:"node"`
	Run         string `json:"run,omitempty"` // 命中所落的 run ID（docx 用得上）
	Start       int    `json:"start"`
	End         int    `json:"end"`
	Context     string `json:"context"`      // 前后各若干字符，供模型判断该改哪处
	ExactAnchor string `json:"exact_anchor"` // 命中节点的原始文本指纹（range 编辑用）
}

// Selector 是一次读取的范围（分页读，避免整篇塞进上下文）。
type Selector struct {
	Node   NodeID `json:"node"`             // 起始节点（空 = 文档根）
	Offset int    `json:"offset,omitempty"` // 在该节点文本投影上的起始 rune 偏移
	Limit  int    `json:"limit,omitempty"`  // 本次最多返回多少 rune（0 = 默认）
}

// Chunk 是一次读取的结果。
type Chunk struct {
	Node  NodeID `json:"node"`
	Text  string `json:"text"`
	Total int    `json:"total"`          // 该节点（或整篇）的字符总数
	Next  int    `json:"next,omitempty"` // 下一页的 Offset；0 = 读完了
}

// Op 是一次编辑的类型。词汇表刻意小：新增只增不改，旧字段语义不动。
type Op string

const (
	// OpReplaceText 用 Text 替换 [Range.Start, Range.End)；Range 为空表示整段替换。
	OpReplaceText Op = "replace_text"
	// OpInsertParagraph 在 Node 之后插入一个段落（Text 为内容）。
	OpInsertParagraph Op = "insert_paragraph"
	// OpDeleteNode 删除整个节点（段落 / 行 / 单元格内容 / 形状）。
	OpDeleteNode Op = "delete_node"
	// OpSetCell 写单元格：Value 或 Formula 二选一（公式与值分开，避免把公式写成文本）。
	OpSetCell Op = "set_cell"
)

// TextRange 是节点文本投影上的字符区间（rune，左闭右开）。
type TextRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// PatchOp 是一次编辑操作。
type PatchOp struct {
	Op   Op     `json:"op"`
	Node NodeID `json:"node"`
	// Anchor 是创建操作时看到的节点文本指纹（见 Fingerprint）。非空则校验，
	// 不匹配即整批拒绝——"文档变了就别照着旧地图改"。
	// 注意：Fingerprint 会折叠空白，带 Range 的精确编辑应优先用 ExactAnchor。
	Anchor string `json:"anchor,omitempty"`
	// ExactAnchor 是节点原始文本投影的指纹（见 ExactFingerprint）。非空则校验；
	// 它不折叠空白，适合 range 编辑——空白变化也会让旧坐标失效。
	ExactAnchor string            `json:"exact_anchor,omitempty"`
	Range       *TextRange        `json:"range,omitempty"`
	Text        string            `json:"text,omitempty"`
	Value       string            `json:"value,omitempty"`
	Formula     string            `json:"formula,omitempty"`
	Style       map[string]string `json:"style,omitempty"`
}

// DiffEntry 是一条可回放的变更：改前 / 改后（截断到可读长度）。
type DiffEntry struct {
	Op     Op     `json:"op"`
	Node   NodeID `json:"node"`
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

// PatchResult 是一次 patch 的结果。
type PatchResult struct {
	Applied  int         `json:"applied"`            // 实际生效的操作数
	Diff     []DiffEntry `json:"diff,omitempty"`     // 逐条变更（给模型与审计看）
	Warnings []string    `json:"warnings,omitempty"` // 降级 / 无法验证等需要外显的事
}

// Document 是一个已打开的文档。实现必须做到：
//   - Patch 只改目标节点所在的那一段字节（其余 part 逐字节不变）；
//   - Patch 失败时不留半成品（要么全成，要么原样返回）；
//   - Bytes 返回的是可直接落盘的完整文件。
//
// 约定：Document 不是并发安全的——同一份文件的写由存储层的版本号串行化。
type Document interface {
	Kind() Kind
	Outline(ctx context.Context, depth int) (Outline, error)
	Find(ctx context.Context, q Query) ([]Match, error)
	ReadRange(ctx context.Context, sel Selector) (Chunk, error)
	Patch(ctx context.Context, ops []PatchOp) (PatchResult, error)
	Bytes() []byte
	// Digest 是整篇可见文本的规范化指纹：服务层把它当一个"整篇版本号"用——
	// outline / read_range / find 返回它，patch 可带上它做乐观锁（对不上就整批拒绝）。
	Digest() string
	// ExactDigest 是未折叠空白的整篇文本指纹。带 rune 区间的编辑应拿它做版本校验：
	// 规范化 digest 对空白变化不敏感，不能证明旧坐标仍然指同一段文字。
	ExactDigest() string
}

// Opener 打开一份文档（data 是完整文件字节）。
type Opener func(data []byte) (Document, error)

var (
	registryMu sync.RWMutex
	registry   = map[Kind]Opener{}
)

// Register 由各格式实现在 init 里调用（避免 docmodel 反向依赖具体格式）。
func Register(kind Kind, fn Opener) {
	if kind == "" || fn == nil {
		return
	}
	registryMu.Lock()
	registry[kind] = fn
	registryMu.Unlock()
}

// Open 按类型打开文档。
func Open(kind Kind, data []byte) (Document, error) {
	registryMu.RLock()
	fn, ok := registry[kind]
	registryMu.RUnlock()
	if !ok {
		return nil, ErrUnsupported
	}
	return fn(data)
}

// Kinds 返回已注册的类型（排序，便于日志与测试断言）。
func Kinds() []Kind {
	registryMu.RLock()
	out := make([]Kind, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	registryMu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// 以下是服务层要映射成"可直接给模型看"的哨兵错误。
var (
	ErrUnsupported  = errors.New("不支持的文档类型")
	ErrNodeNotFound = errors.New("节点不存在")
	ErrNoMatch      = errors.New("没有匹配")
	ErrAmbiguous    = errors.New("匹配到多处，需要指定节点 ID")
	ErrStale        = errors.New("文档内容已变化（anchor 不匹配），请重新读取")
	ErrBadOp        = errors.New("编辑操作非法")
)

// Fingerprint 是节点文本的短指纹（sha256 前 8 字节的十六进制，16 字符）。
// 用途：patch 时校验"要改的还是当初看到的那一段"——版本漂移后照着旧地图改
// 是文档编辑里最贵的一类事故。
func Fingerprint(text string) string {
	sum := sha256.Sum256([]byte(normalizeSpace(text)))
	return hex.EncodeToString(sum[:8])
}

// ExactFingerprint 与 Fingerprint 同长度，但不折叠任何字符。
// 任何文字或空白变化都会改变结果，适合校验带 rune 区间的编辑坐标。
func ExactFingerprint(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:8])
}

// normalizeSpace 折叠空白：排版差异（多余空格 / 换行）不该让锚点失效。
func normalizeSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

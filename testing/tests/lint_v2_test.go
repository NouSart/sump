package tests

// memory V2 的静态 lint（R-011 §2.4 / §2.5 / §1 特征 3、20 / §5 DoD 6）。
//
// 这一组用例不启动服务、不连网、不访问数据库：只读仓库源码，把「一词多义（概念漂移）/
// 多处真相（重复实现）/ 许愿残留 / 新增第三方依赖」钉成断言。失败信息带 文件:行 与命中值，
// 并附一条处置建议，方便 lead 直接决定是改代码还是改验收标准。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ---------- 通用扫描辅助 ----------

// sourceHit 是一条静态命中（文件 + 行号 + 该行文本）。
type sourceHit struct {
	File string // 相对仓库根、斜杠分隔
	Line int
	Text string
}

func (h sourceHit) String() string {
	return strconv.Itoa(h.Line) + ": " + h.Text
}

// relToRoot 把绝对路径转成相对仓库根的斜杠路径（失败信息只暴露仓库内路径）。
func relToRoot(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

// goFiles 递归收集目录下全部 .go 文件（含测试文件）。
func goFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s 失败: %v", dir, err)
	}
	sort.Strings(files)
	return files
}

// nonTestGoFiles 只要实现文件（lint 针对代码，不针对测试夹具；测试里的 id 生成不算实现）。
func nonTestGoFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, path := range goFiles(t, dir) {
		if !strings.HasSuffix(path, "_test.go") {
			out = append(out, path)
		}
	}
	return out
}

// parseFile 解析一个 Go 文件；解析失败直接判失败（lint 不能被语法错误蒙混过去）。
func parseFile(t *testing.T, path string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("解析 %s 失败: %v", path, err)
	}
	return fset, file
}

// literalHits 在文件集合里找「字符串字面量的值包含 needle」的位置。
// 用 AST 而不是文本匹配：注释里的解释性提及不算（S-001 §3.9 只约束可执行词表）。
func literalHits(t *testing.T, root string, files []string, needle string) []sourceHit {
	t.Helper()
	var hits []sourceHit
	for _, path := range files {
		fset, file := parseFile(t, path)
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil || !strings.Contains(value, needle) {
				return true
			}
			pos := fset.Position(lit.Pos())
			hits = append(hits, sourceHit{File: relToRoot(root, path), Line: pos.Line, Text: value})
			return true
		})
	}
	return hits
}

// callSites 在文件集合里找「调用某个名字的函数/方法」的位置（如 NewID / CreateCall）。
// 只看可执行调用，不看注释与字符串。
func callSites(t *testing.T, root string, files []string, funcName string) []sourceHit {
	t.Helper()
	var hits []sourceHit
	for _, path := range files {
		fset, file := parseFile(t, path)
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != funcName {
				return true
			}
			pos := fset.Position(call.Pos())
			hits = append(hits, sourceHit{File: relToRoot(root, path), Line: pos.Line, Text: funcName + "()"})
			return true
		})
	}
	return hits
}

// ---------- 词汇 lint（概念漂移） ----------

// TestLintV2Vocabulary 扫描 memory 全部 Go 源码与协议载荷，禁止出现词汇表之外的状态字符串。
//
// 规则（R-011 §2.4）：只禁「字符串字面量恰好等于禁用状态词」；注释里的解释性提及允许
// （比如说明「为什么不叫 X」），因为注释不参与线上取值。禁用词用拼接构造，避免 lint 代码
// 自己成为命中项。
func TestLintV2Vocabulary(t *testing.T) {
	root := repoRootForDocs(t)
	files := goFiles(t, filepath.Join(root, "src", "memory"))
	// 协议载荷是线上词汇的另一半：状态词一旦在这里出现，全链路都会跟着漂。
	payloads := filepath.Join(root, "src", "protocol", "payloads_v2.go")
	if _, err := os.Stat(payloads); err != nil {
		t.Fatalf("缺少协议载荷 %s: %v", relToRoot(root, payloads), err)
	}
	files = append(files, payloads)

	// 反自指：不在源码里留下完整禁用词。
	forbidden := []string{"que" + "ued", "sett" + "led"}

	hits := 0
	for _, path := range files {
		fset, file := parseFile(t, path)
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, bad := range forbidden {
				if value != bad {
					continue
				}
				hits++
				pos := fset.Position(lit.Pos())
				t.Errorf("%s:%d:%d 出现禁用状态词字面量 %q：S-001 §3.9 的词汇表只有一套，状态字符串必须取表中值",
					relToRoot(root, path), pos.Line, pos.Column, value)
			}
			return true
		})
	}
	if len(files) < 20 {
		t.Fatalf("只扫到 %d 个文件，扫描范围可能失效（词汇 lint 不能空转）", len(files))
	}
	if hits == 0 {
		t.Logf("词汇 lint 通过：扫描 %d 个文件，未发现禁用状态词字面量", len(files))
	}
}

// ---------- 唯一实现 lint（重复实现 / 多处真相） ----------

// TestLintV2SingleImplementation 守住三条「只有一处」的事实（R-011 §2.5）：
//  1. works 表定义只允许 store 包一处（不要第二个模块各建一份）；
//  2. call_id 只允许由 store.CreateCall 里的一个 NewID 生成（server 不得自己造 id）；
//  3. calls 表的 INSERT 只允许一处（状态落点的唯一入口）。
func TestLintV2SingleImplementation(t *testing.T) {
	root := repoRootForDocs(t)
	storeDir := filepath.Join(root, "src", "memory", "store")
	serverDir := filepath.Join(root, "src", "memory", "server")
	storeFiles := nonTestGoFiles(t, storeDir)
	serverFiles := nonTestGoFiles(t, serverDir)

	// 1) works 表定义唯一。
	workDDL := literalHits(t, root, storeFiles, "CREATE TABLE IF NOT EXISTS works")
	if len(workDDL) != 1 {
		paths := make([]string, 0, len(workDDL))
		for _, hit := range workDDL {
			paths = append(paths, hit.File)
		}
		t.Errorf("CREATE TABLE IF NOT EXISTS works 在 store 包应只出现 1 次（works 只能有一个权威定义），实际 %d 次：%v",
			len(workDDL), paths)
	}

	// 2) calls 表的 INSERT 唯一（call_id 的落库入口）。
	callInsert := literalHits(t, root, storeFiles, "INSERT INTO calls")
	if len(callInsert) != 1 {
		paths := make([]string, 0, len(callInsert))
		for _, hit := range callInsert {
			paths = append(paths, hit.File)
		}
		t.Errorf("calls 表的 INSERT 应只出现 1 次（状态只允许一个落库入口），实际 %d 次：%v", len(callInsert), paths)
	}

	// 3) call_id 生成点唯一：store/calls.go 恰好 1 次 NewID；其余 store 的 NewID 不属于 calls 账本。
	var callsNewIDs []sourceHit
	for _, hit := range callSites(t, root, storeFiles, "NewID") {
		if strings.HasSuffix(hit.File, "/calls.go") {
			callsNewIDs = append(callsNewIDs, hit)
			continue
		}
		t.Logf("旁证：%s 也调用 NewID，但不生成 call_id（a2a 队列等其它 id；calls 账本只认 CreateCall）", hit.File)
	}
	if len(callsNewIDs) != 1 {
		t.Errorf("store/calls.go 里生成 call_id 的 NewID 调用应恰好 1 处，实际 %d 处：%v", len(callsNewIDs), callsNewIDs)
	}

	// 4) server 包不得自己造 call_id：其它 id（如 snapshot_id）允许，但必须显式登记原因。
	newIDs := callSites(t, root, serverFiles, "NewID")
	allowedNewID := map[string]string{
		"context_v2.go": "快照 id（snapshot_id，S-001 §4.4）；不是 call_id",
	}
	for _, hit := range newIDs {
		base := filepath.Base(hit.File)
		if reason, ok := allowedNewID[base]; ok {
			t.Logf("白名单：%s:%d 的 NewID 用于 %s", hit.File, hit.Line, reason)
			continue
		}
		t.Errorf("%s:%d 直接调用 NewID：call_id 只允许经 store.CreateCall 一处生成（S-001 §3.0.2），server 不得自己造 id",
			hit.File, hit.Line)
	}

	// 5) server 里对 call 账本的唯一写入口是 calls 相关文件。
	for _, hit := range callSites(t, root, serverFiles, "CreateCall") {
		if !strings.Contains(filepath.Base(hit.File), "calls") {
			t.Errorf("%s:%d 调用 CreateCall：call_open 的落库入口应集中在 calls 相关文件", hit.File, hit.Line)
		}
	}
}

// ---------- 许愿残留 lint ----------

// v2ImplementationFiles 是本次 V2 新增的实现文件（对比 main 的 merge-base，不含 *_test.go）。
// 目的不是维护一份清单，而是让「未做项」无处躲：没实现的东西要么进 S-001 §14 / TODO.md，
// 要么现在就把标记删掉，不能以 TODO 的形式留在新增代码里冒充已完成（R-011 §1 特征 3、16）。
var v2ImplementationFiles = []string{
	"src/memory/assemble/assemble.go",
	"src/memory/assemble/render.go",
	"src/memory/assemble/tools.go",
	"src/memory/assemble/types.go",
	"src/memory/server/calls_v2.go",
	"src/memory/server/context_v2.go",
	"src/memory/server/store_v2.go",
	"src/memory/server/owner_client.go", // owner 握手/对账的可注入契约（与 calls_v2 同批）
	"src/memory/store/calls.go",
	"src/memory/store/record_v2.go", // v2 唯一的原子落库入口（取代原 receipts.go 两阶段写）
	"src/memory/store/schema_v2.go",
	"src/memory/store/seq_counters.go",
	"src/memory/store/snapshots.go",
	"src/memory/shared/version.go", // 共享库 user_version 闸门（不是 *_v2 文件，动态兜底扫不到）
	"src/protocol/jcs/jcs.go",
	"src/protocol/jcs/parse.go",
	"src/protocol/payloads_v2.go",
}

// wishfulMarker 只认「注释标记」形态：TODO( / TODO: / TODO： / TODO 空白 / FIXME 同理 / not implemented。
// 特意不匹配 “TODO.md”——那是在引用文档，不是留下未做标记。
var wishfulMarker = regexp.MustCompile(`(?i)(?:TODO|FIXME)(?:\(|:|：|\s)|not\s+implemented`)

// TestLintV2NoWishfulStubs 新增 V2 实现文件里不得有 TODO / FIXME / not implemented。
//
// 时间敏感说明：断言对象是下面的「新增 V2 实现文件清单」（加上任何 *_v2.go），
// 不是整个仓库；所以某天在新增文件里补一句 TODO 注释，这条就会红——这是有意的，
// 许愿标记不该有「历史遗留」豁免。失败时按下面的处置建议改，而不是删用例。
//
// 若失败：这些标记必须改写成 S-001 §14「不做」或 TODO.md 的登记项，并从代码里删掉；
// 不允许「先合后补」的空壳（R-011 §1 特征 3、16）。
func TestLintV2NoWishfulStubs(t *testing.T) {
	root := repoRootForDocs(t)
	files := append([]string(nil), v2ImplementationFiles...)

	// 动态兜底：以后新增的 *_v2.go 自动纳入，避免清单过期漏扫。
	for _, path := range goFiles(t, filepath.Join(root, "src", "memory")) {
		base := filepath.Base(path)
		if strings.Contains(base, "_v2") && !strings.HasSuffix(base, "_test.go") {
			files = append(files, relToRoot(root, path))
		}
	}
	sort.Strings(files)
	seen := map[string]bool{}
	var unique []string
	for _, f := range files {
		if !seen[f] {
			seen[f] = true
			unique = append(unique, f)
		}
	}
	files = unique

	var hits []string
	for _, rel := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读 %s 失败: %v（V2 文件清单过期？）", rel, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if wishfulMarker.MatchString(line) {
				hits = append(hits, rel+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
	}
	if len(hits) > 0 {
		for _, hit := range hits {
			t.Errorf("许愿残留（新增 V2 文件不得留 TODO/FIXME/not implemented）：%s", hit)
		}
		t.Logf("处置建议：未做项写进 S-001 §14 或 TODO.md 并说明边界，然后删掉代码里的标记；" +
			"不要以 TODO 注释冒充「已设计、待接线」")
	}
	if len(files) < len(v2ImplementationFiles) {
		t.Fatalf("V2 文件清单只读到 %d 个（应至少 %d 个），扫描范围失效", len(files), len(v2ImplementationFiles))
	}
}

// ---------- 依赖 lint（不得新增第三方依赖） ----------

// frozenMemoryRequires 是 V2 分支起点时 memory 模块的 require 基线（提交 5d92fa9 的 go.mod），
// 仅在没有 git 可用的环境里作为兜底——正常情况下以 git show HEAD:src/memory/go.mod 为准。
var frozenMemoryRequires = []string{
	"github.com/NouSart/sump/protocol",
	"github.com/NouSart/sump/service",
	"github.com/NouSart/sump/spv2",
	"github.com/dustin/go-humanize",
	"github.com/google/uuid",
	"github.com/gorilla/websocket",
	"github.com/mattn/go-isatty",
	"github.com/ncruces/go-strftime",
	"github.com/oklog/ulid/v2",
	"github.com/remyoudompheng/bigfft",
	"github.com/vmihailenco/msgpack/v5",
	"github.com/vmihailenco/tagparser/v2",
	"golang.org/x/sys",
	"modernc.org/libc",
	"modernc.org/mathutil",
	"modernc.org/memory",
	"modernc.org/sqlite",
}

// parseGoModRequires 从 go.mod 文本里解析 require 的模块路径（含 indirect，忽略注释）。
func parseGoModRequires(text string) []string {
	var out []string
	inBlock := false
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if inBlock {
			if line == ")" {
				inBlock = false
				continue
			}
			if strings.HasPrefix(line, "//") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) >= 1 {
				out = append(out, fields[0])
			}
			continue
		}
		if line == "require (" {
			inBlock = true
			continue
		}
		if strings.HasPrefix(line, "require ") {
			fields := strings.Fields(strings.TrimPrefix(line, "require "))
			if len(fields) >= 1 {
				out = append(out, fields[0])
			}
		}
	}
	sort.Strings(out)
	return out
}

// gitShowFile 从本仓库读一个文件在某个 git 版本里的内容；环境没有 git / 不是仓库时返回错误。
func gitShowFile(root, spec string) (string, error) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return "", err
	}
	out, err := exec.Command(gitBin, "-C", root, "show", spec).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// TestLintV2NoNewDependencies 证明这次 V2 没有偷偷加第三方依赖（DoD 6）。
// 以 git HEAD 的 memory/go.mod 为基线（当前分支首次提交 V2 代码时的状态）；
// 没有 git 时退回内置基线并在日志里说明，避免用例空转。
func TestLintV2NoNewDependencies(t *testing.T) {
	root := repoRootForDocs(t)
	currentPath := filepath.Join(root, "src", "memory", "go.mod")
	currentData, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", relToRoot(root, currentPath), err)
	}
	current := parseGoModRequires(string(currentData))
	if len(current) == 0 {
		t.Fatal("当前 go.mod 解析不出任何 require，lint 无法校验")
	}

	baselineText, gitErr := gitShowFile(root, "HEAD:src/memory/go.mod")
	baseline := parseGoModRequires(baselineText)
	baselineSource := "git HEAD:src/memory/go.mod"
	if gitErr != nil || len(baseline) == 0 {
		baseline = frozenMemoryRequires
		baselineSource = "内置基线（提交 5d92fa9）"
		t.Logf("无法从 git 取 HEAD 基线（%v），退回内置基线校验", gitErr)
	}

	known := map[string]bool{}
	for _, mod := range baseline {
		known[mod] = true
	}
	var added []string
	for _, mod := range current {
		if !known[mod] {
			added = append(added, mod)
		}
	}
	if len(added) > 0 {
		t.Errorf("memory 模块新增了 require（R-011 §1 特征 20 / DoD 6：不得新增第三方依赖）：%v；"+
			"如确需新增，先改验收标准与 S-001，并在此登记原因", added)
	}

	// 反向核对（不判失败，只提示）：基线里的依赖被删说明可能动了不该动的地方。
	existing := map[string]bool{}
	for _, mod := range current {
		existing[mod] = true
	}
	for _, mod := range baseline {
		if !existing[mod] {
			t.Logf("提示：基线 require %q 已不在当前 go.mod 中（新增依赖之外的变化请复核）", mod)
		}
	}
	t.Logf("依赖 lint 通过：当前 %d 个 require，基线 %d 个（%s）", len(current), len(baseline), baselineSource)
}

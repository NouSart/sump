// sump-repair 是共享经验库的存量清理工具（一次性，不进服务进程）。
//
//	默认预演：只报告"哪些列还有多少格不合规"，不写盘。
//	加 -apply 才真洗；洗完复检不归零就整体回滚。
//	加 -audit 走第二把尺（audit 包，与清洗谓词无共享），只读不写；
//	配 -known <file> 时用本机凭据文件比对已知值。
//
// -audit 的闸门口径是"有**未裁定**的命中才破"：命中带形状摘要（几段、各长多少、熵多少），
// 压成键之后与 adjudicated.json 里人已逐处读过的清单对账。两个开关：
//
//	-adjudicated <path>  换一份白名单（默认空 = 用编进二进制的 audit/adjudicated.json）
//	-no-adjudicated      关掉白名单，输出 Scan 原样结果（复现无条件基线 / 第一遍抄 key）
//
// 两个开关互斥，同时给直接报错退出。登记新裁定的引导顺序：先用 -no-adjudicated 跑一遍，
// 拿到 String() 形式的输出，人逐处读过，再把里面的 key 逐字抄进 adjudicated.json，重编。
// JSON 不是手写的——键只能由 Key() 算，抄错一位就是未裁定。
// 注意：-audit 目前要求库里有 scope 的全部表，缺表时 Scan 当场报错（exit 1），
// 到不了闸门——换一个没有这些表的库不是"跳过"，是失败。
//
// 用法（先停掉 memory 服务，避免两个写者抢同一张表；-audit 是只读的，可以不停）：
//
//	cd src/memory
//	go run ./cmd/sump-repair -db ../../data/experience.db                     # 预演
//	go run ./cmd/sump-repair -db ../../data/experience.db -audit -known ../../data/qq-secret.json
//	                                                                           # 独立复检（带白名单对账）
//	go run ./cmd/sump-repair -db ../../data/experience.db -audit -no-adjudicated
//	                                                                           # 无条件口径（修前基线，必须 ≥1）
//	go run ./cmd/sump-repair -db ../../data/experience.db -apply              # 写盘
//	go run ./cmd/sump-repair -db ../../data/experience.db -apply -audit -known ../../data/qq-secret.json
//	                                                                           # 洗完立刻两把尺一起复检
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"

	_ "modernc.org/sqlite"

	"github.com/NouSart/sump/memory/audit"
	"github.com/NouSart/sump/memory/repair"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run 是 sump-repair 的全部逻辑：参数与两条输出流都由调用方给，所以退出码与文案能在测试里
// 逐条断言，不必起子进程，也不用碰真实数据。
// 退出码口径不变：0 = 放行，1 = 尺子不归零（或未收口的维护问题），2 = 用法与环境错误。
func run(args []string, stdout, stderr io.Writer) int {
	// 独立的 FlagSet 而不是全局 flag：测试要在同一进程里跑多次，全局状态会互相污染。
	fs := flag.NewFlagSet("sump-repair", flag.ContinueOnError)
	// 解析错误与 Usage 必须落进注入的 stderr——默认写法是直连进程真实 stderr，
	// 那样"用法错误返回 2 并留下可读说明"这条断言其实什么都抓不到。
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "data/experience.db", "共享经验库路径")
	apply := fs.Bool("apply", false, "真正写盘（默认只预演）")
	auditOnly := fs.Bool("audit", false, "只跑独立复检（audit 包，与清洗谓词不共享）；配 -apply 则先清洗再复检")
	knownPath := fs.String("known", "", "已知凭据 JSON 文件（递归取字符串叶子，供 -audit 第一路探针；不给就跳过这一路）")
	adjudicatedPath := fs.String("adjudicated", "", "已裁定白名单 JSON 路径（不给就用编进二进制的那份 audit/adjudicated.json）")
	noAdjudicated := fs.Bool("no-adjudicated", false, "关闭已裁定白名单，-audit 输出 Scan 原样结果（复现无条件基线 / 第一遍抄 key）")
	if err := fs.Parse(args); err != nil {
		// flag 包已经把错误与 Usage 写进 stderr 了，这里只管返回码。
		return 2
	}

	// 互斥要说破，不要由实现顺序决定谁赢：一个换清单、一个关清单，同时给没有唯一答案。
	// 必须在开库之前判：否则这条会在"默认库不存在"那条 2 里被冒充成已经验过。
	if *noAdjudicated && *adjudicatedPath != "" {
		fmt.Fprintln(stderr, "-adjudicated 与 -no-adjudicated 互斥：前者换一份白名单，后者关掉白名单")
		return 2
	}

	if _, err := os.Stat(*dbPath); err != nil {
		fmt.Fprintf(stderr, "打不开共享库 %s: %v\n", *dbPath, err)
		return 2
	}
	var known []string
	if *knownPath != "" {
		vals, skipped, err := audit.KnownValues(*knownPath)
		if err != nil {
			fmt.Fprintf(stderr, "%v\n", err)
			return 2
		}
		known = vals
		fmt.Fprintf(stderr, "已知凭据：从 %s 取到 %d 个够长的字符串叶子（值不打印）", *knownPath, len(known))
		if skipped > 0 {
			fmt.Fprintf(stderr, "，另有 %d 个数字/布尔/null 叶子未纳入——凭据要写成那样的话这一路看不见", skipped)
		}
		fmt.Fprintln(stderr)
	}

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "打开共享库失败: %v\n", err)
		return 2
	}
	defer db.Close()
	// 与 shared.Open 一样串行化：SQLite 单写者，事务里套并发读只会自己等自己。
	db.SetMaxOpenConns(1)

	// -audit 且不 -apply 时是纯只读尺子；只有显式 -apply 才先动库。
	if !*auditOnly || *apply {
		rep, err := repair.Clean(db, *apply)
		if err != nil {
			fmt.Fprintf(stderr, "清理失败: %v\n", err)
			return 1
		}
		fmt.Fprint(stdout, rep.Summary())
		if len(rep.Remaining) > 0 {
			fmt.Fprintln(stderr, "复检仍命中：这些格清理规则够不到，需要人工看（报告不含原值）")
			return 1
		}
		if !*auditOnly {
			return 0
		}
	}

	hits, err := audit.Scan(db, known)
	if err != nil {
		fmt.Fprintf(stderr, "独立复检失败: %v\n", err)
		return 1
	}

	// 关掉白名单 = 修前那个无条件口径，Scan 命中几处就报几处。
	// 留着它是为了两件事：第一遍抄 key，以及"脏库必破闸"这条基线性质可复现。
	if *noAdjudicated {
		for _, f := range hits {
			fmt.Fprintln(stdout, f)
		}
		fmt.Fprintf(stdout, "独立复检命中 %d 处（-no-adjudicated：不查已裁定白名单，报告只出位置、形状摘要与键，不含原值）\n", len(hits))
		if len(hits) > 0 {
			fmt.Fprintln(stderr, "第二把尺不归零：逐处人工读过再决定，不许为了让它闭嘴去改弱探针")
			return 1
		}
		return 0
	}

	var adj []audit.Adjudicated
	if *adjudicatedPath != "" {
		adj, err = audit.LoadAdjudicated(*adjudicatedPath)
	} else {
		adj, err = audit.DefaultAdjudicated()
	}
	if err != nil {
		// 白名单读不动 / 不合法，绝不能当成"没有已裁定的条目"继续跑：
		// 那等于拿一份坏清单去判"全都未裁定"，报出来的红是假的，人反而会去改探针让它闭嘴。
		fmt.Fprintf(stderr, "%v\n", err)
		return 2
	}
	rowSet, err := audit.NewRowSet(db, adj)
	if err != nil {
		fmt.Fprintf(stderr, "核对已裁定记录失败: %v\n", err)
		return 1
	}
	novel, stale, skipped := audit.Filter(hits, adj, rowSet.Has)

	// 先报安全问题（未裁定），再报维护问题（过期），两份都报完才决定退出码：
	// 一行被改写时 novel 与 stale 同时成立，只报其中一个会把另一个吞掉。
	for _, f := range novel {
		fmt.Fprintln(stdout, f)
	}
	for _, a := range stale {
		fmt.Fprintf(stdout, "过期裁定条目：%s\n", a)
	}
	if len(skipped) > 0 {
		fmt.Fprintf(stdout, "跳过 %d 条已裁定记录：它们指的行不在当前库里（换了库或行已删），不算命中也不算失败\n", len(skipped))
	}
	fmt.Fprintf(stdout, "独立复检命中 %d 处：未裁定 %d 处、过期裁定 %d 处、已裁定放行 %d 处（报告只出位置、形状摘要与键，不含原值）\n",
		len(hits), len(novel), len(stale), len(hits)-len(novel))
	if len(novel) > 0 {
		fmt.Fprintln(stderr, "第二把尺不归零：有未裁定的命中，逐处人工读过再决定，不许为了让它闭嘴去改弱探针")
		return 1
	}
	if len(stale) > 0 {
		fmt.Fprintln(stderr, "白名单里有对不上当前库的条目（行被改写过或已洗掉）：跑一遍 -no-adjudicated，逐处读过再把 key 抄回 adjudicated.json")
		return 1
	}
	fmt.Fprintf(stdout, "命中 %d 处，全部已裁定\n", len(hits))
	return 0
}

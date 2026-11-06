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
	"os"

	_ "modernc.org/sqlite"

	"github.com/NouSart/sump/memory/audit"
	"github.com/NouSart/sump/memory/repair"
)

func main() {
	dbPath := flag.String("db", "data/experience.db", "共享经验库路径")
	apply := flag.Bool("apply", false, "真正写盘（默认只预演）")
	auditOnly := flag.Bool("audit", false, "只跑独立复检（audit 包，与清洗谓词不共享）；配 -apply 则先清洗再复检")
	knownPath := flag.String("known", "", "已知凭据 JSON 文件（递归取字符串叶子，供 -audit 第一路探针；不给就跳过这一路）")
	adjudicatedPath := flag.String("adjudicated", "", "已裁定白名单 JSON 路径（不给就用编进二进制的那份 audit/adjudicated.json）")
	noAdjudicated := flag.Bool("no-adjudicated", false, "关闭已裁定白名单，-audit 输出 Scan 原样结果（复现无条件基线 / 第一遍抄 key）")
	flag.Parse()

	// 互斥要说破，不要由实现顺序决定谁赢：一个换清单、一个关清单，同时给没有唯一答案。
	if *noAdjudicated && *adjudicatedPath != "" {
		fmt.Fprintln(os.Stderr, "-adjudicated 与 -no-adjudicated 互斥：前者换一份白名单，后者关掉白名单")
		os.Exit(2)
	}

	if _, err := os.Stat(*dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "打不开共享库 %s: %v\n", *dbPath, err)
		os.Exit(2)
	}
	var known []string
	if *knownPath != "" {
		vals, skipped, err := audit.KnownValues(*knownPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(2)
		}
		known = vals
		fmt.Fprintf(os.Stderr, "已知凭据：从 %s 取到 %d 个够长的字符串叶子（值不打印）", *knownPath, len(known))
		if skipped > 0 {
			fmt.Fprintf(os.Stderr, "，另有 %d 个数字/布尔/null 叶子未纳入——凭据要写成那样的话这一路看不见", skipped)
		}
		fmt.Fprintln(os.Stderr)
	}

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开共享库失败: %v\n", err)
		os.Exit(2)
	}
	defer db.Close()
	// 与 shared.Open 一样串行化：SQLite 单写者，事务里套并发读只会自己等自己。
	db.SetMaxOpenConns(1)

	// -audit 且不 -apply 时是纯只读尺子；只有显式 -apply 才先动库。
	if !*auditOnly || *apply {
		rep, err := repair.Clean(db, *apply)
		if err != nil {
			fmt.Fprintf(os.Stderr, "清理失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(rep.Summary())
		if len(rep.Remaining) > 0 {
			fmt.Fprintln(os.Stderr, "复检仍命中：这些格清理规则够不到，需要人工看（报告不含原值）")
			os.Exit(1)
		}
		if !*auditOnly {
			return
		}
	}

	hits, err := audit.Scan(db, known)
	if err != nil {
		fmt.Fprintf(os.Stderr, "独立复检失败: %v\n", err)
		os.Exit(1)
	}

	// 关掉白名单 = 修前那个无条件口径，Scan 命中几处就报几处。
	// 留着它是为了两件事：外来库上第一遍抄 key，以及"脏库必破闸"这条基线性质可复现。
	if *noAdjudicated {
		for _, f := range hits {
			fmt.Println(f)
		}
		fmt.Printf("独立复检命中 %d 处（-no-adjudicated：不查已裁定白名单，报告只出位置、形状摘要与键，不含原值）\n", len(hits))
		if len(hits) > 0 {
			fmt.Fprintln(os.Stderr, "第二把尺不归零：逐处人工读过再决定，不许为了让它闭嘴去改弱探针")
			os.Exit(1)
		}
		return
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
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	rowSet, err := audit.NewRowSet(db, adj)
	if err != nil {
		fmt.Fprintf(os.Stderr, "核对已裁定记录失败: %v\n", err)
		os.Exit(1)
	}
	novel, stale, skipped := audit.Filter(hits, adj, rowSet.Has)

	// 先报安全问题（未裁定），再报维护问题（过期），两份都报完才决定退出码：
	// 一行被改写时 novel 与 stale 同时成立，只报其中一个会把另一个吞掉。
	for _, f := range novel {
		fmt.Println(f)
	}
	for _, a := range stale {
		fmt.Printf("过期裁定条目：%s\n", a)
	}
	if len(skipped) > 0 {
		fmt.Printf("跳过 %d 条已裁定记录：它们指的行不在当前库里（换了库或行已删），不算命中也不算失败\n", len(skipped))
	}
	fmt.Printf("独立复检命中 %d 处：未裁定 %d 处、过期裁定 %d 处、已裁定放行 %d 处（报告只出位置、形状摘要与键，不含原值）\n",
		len(hits), len(novel), len(stale), len(hits)-len(novel))
	if len(novel) > 0 {
		fmt.Fprintln(os.Stderr, "第二把尺不归零：有未裁定的命中，逐处人工读过再决定，不许为了让它闭嘴去改弱探针")
		os.Exit(1)
	}
	if len(stale) > 0 {
		fmt.Fprintln(os.Stderr, "白名单里有对不上当前库的条目（行被改写过或已洗掉）：跑一遍 -no-adjudicated，逐处读过再把 key 抄回 adjudicated.json")
		os.Exit(1)
	}
	fmt.Printf("命中 %d 处，全部已裁定\n", len(hits))
}

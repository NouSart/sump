// sump-repair 是共享经验库的存量清理工具（一次性，不进服务进程）。
//
//	默认预演：只报告"哪些列还有多少格不合规"，不写盘。
//	加 -apply 才真洗；洗完复检不归零就整体回滚。
//	加 -audit 走第二把尺（audit 包，与清洗谓词无共享），只读不写；
//	配 -known <file> 时用本机凭据文件比对已知值。
//
// 用法（先停掉 memory 服务，避免两个写者抢同一张表；-audit 是只读的，可以不停）：
//
//	cd src/memory
//	go run ./cmd/sump-repair -db ../../data/experience.db                     # 预演
//	go run ./cmd/sump-repair -db ../../data/experience.db -audit -known ../../data/qq-secret.json
//	                                                                           # 独立复检（修前基线，必须 ≥1）
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
	flag.Parse()

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
	for _, f := range hits {
		fmt.Println(f)
	}
	fmt.Printf("独立复检命中 %d 处（报告只出位置与类别，不含原值）\n", len(hits))
	if len(hits) > 0 {
		fmt.Fprintln(os.Stderr, "第二把尺不归零：逐处人工读过再决定，不许为了让它闭嘴去改弱探针")
		os.Exit(1)
	}
}

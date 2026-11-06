// sump-repair 是共享经验库的存量清理工具（一次性，不进服务进程）。
//
//	默认预演：只报告"哪些列还有多少格不合规"，不写盘。
//	加 -apply 才真洗；洗完复检不归零就整体回滚。
//
// 用法（先停掉 memory 服务，避免两个写者抢同一张表）：
//
//	cd src/memory
//	go run ./cmd/sump-repair -db ../../data/experience.db            # 预演
//	go run ./cmd/sump-repair -db ../../data/experience.db -apply     # 写盘
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"

	_ "modernc.org/sqlite"

	"github.com/NouSart/sump/memory/repair"
)

func main() {
	dbPath := flag.String("db", "data/experience.db", "共享经验库路径")
	apply := flag.Bool("apply", false, "真正写盘（默认只预演）")
	flag.Parse()

	if _, err := os.Stat(*dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "打不开共享库 %s: %v\n", *dbPath, err)
		os.Exit(2)
	}
	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开共享库失败: %v\n", err)
		os.Exit(2)
	}
	defer db.Close()
	// 与 shared.Open 一样串行化：SQLite 单写者，事务里套并发读只会自己等自己。
	db.SetMaxOpenConns(1)

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
}

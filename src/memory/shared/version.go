package shared

// 共享经验库的版本闸门（S-001 §11.1 / D3「不迁移、重置」）。
//
// 与会话库同一条规矩：**任何 DDL 之前**先只读判断库的状态，
// 版本不对就拒绝启动，不做任何形式的自动迁移或自动删除。
// 共享库里的内容按红线是可丢弃、可重建的提炼物，但「悄悄换掉数据格式」
// 会让旧进程读新库、新进程读旧库，两边的结论都不可信——所以宁可拒绝。

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SchemaVersion 是共享经验库当前的数据格式版本。
// 改了表结构（含 longterm / memnet 的表）就必须 +1，并配套更新重置说明。
const SchemaVersion = 1

// ErrSchemaVersionMismatch 是共享库版本闸门的哨兵错误，用 errors.Is 判定。
var ErrSchemaVersionMismatch = errors.New("共享库 schema 版本不匹配")

// SchemaVersionMismatchError 带上路径与两个版本号，错误信息直接给人看。
type SchemaVersionMismatchError struct {
	Path     string // 共享库文件路径
	Current  int    // 库里记录的版本（PRAGMA user_version）
	Expected int    // 本程序期望的版本（SchemaVersion）
}

func (e *SchemaVersionMismatchError) Error() string {
	return fmt.Sprintf("共享库 %s 的 schema 版本不匹配：当前版本 %d，期望版本 %d；已拒绝启动，未写入任何数据",
		e.Path, e.Current, e.Expected)
}

// Is 让 errors.Is(err, ErrSchemaVersionMismatch) 成立。
func (e *SchemaVersionMismatchError) Is(target error) bool {
	return target == ErrSchemaVersionMismatch
}

// schemaState 是只读预检得到的共享库状态。
type schemaState struct {
	exists    bool // 库文件是否存在
	version   int  // PRAGMA user_version
	hasTables bool // sqlite_master 里是否已有用户表
}

// CheckPath 只做 os.Stat 与只读的 user_version 读取，绝不建表、绝不写库、绝不创建文件。
// 给 main 的启动预检用（S-001 §11.1）：文件不存在返回 (false, 0, nil)。
func CheckPath(path string) (exists bool, version int, err error) {
	if _, statErr := os.Stat(path); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("检查共享库 %s: %w", path, statErr)
	}
	db, err := openReadOnly(path)
	if err != nil {
		return true, 0, err
	}
	defer func() { _ = db.Close() }()
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return true, 0, fmt.Errorf("读取共享库 %s 的 schema 版本: %w", path, err)
	}
	return true, version, nil
}

// inspectSchema 是 Open 的只读预检：文件是否存在、版本号、是否已有表。
func inspectSchema(path string) (schemaState, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return schemaState{}, nil
		}
		return schemaState{}, fmt.Errorf("检查共享库 %s: %w", path, err)
	}
	db, err := openReadOnly(path)
	if err != nil {
		return schemaState{}, err
	}
	defer func() { _ = db.Close() }()

	state := schemaState{exists: true}
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&state.version); err != nil {
		return schemaState{}, fmt.Errorf("读取共享库 %s 的 schema 版本: %w", path, err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&n); err != nil {
		return schemaState{}, fmt.Errorf("读取共享库 %s 的表清单: %w", path, err)
	}
	state.hasTables = n > 0
	return state, nil
}

// allowWrite 判定这份库能不能写：
//   - 不存在 = 新库，允许；
//   - user_version == SchemaVersion = 同版本，允许（建表语句全是 IF NOT EXISTS，幂等）；
//   - user_version == 0 且库里没有任何表 = 空文件，允许按新库处理；
//   - 其余（0 但已有表 = 旧库；或版本号更高）一律拒绝。
func allowWrite(state schemaState) bool {
	if !state.exists {
		return true
	}
	if state.version == SchemaVersion {
		return true
	}
	return state.version == 0 && !state.hasTables
}

// openReadOnly 以只读方式打开共享库（file: URI + mode=ro）。
// mode=ro 下 SQLite 直接拒绝任何写语句，也绝不创建文件——比「只读事务」更硬的保证。
func openReadOnly(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("只读打开共享库 %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("只读打开共享库 %s: %w", path, err)
	}
	return db, nil
}

// applySchema 在单个事务里建表并写入 user_version：
// 中途失败不会留下「建了表但没版本号」的库（那样下次启动会被自己的闸门锁死）。
// longterm / memnet 的表在建版本之后由各自的 Open 幂等补齐——它们本来就每次启动都跑。
func applySchema(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("开始共享库 schema 事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(schema); err != nil {
		return fmt.Errorf("建共享库表失败: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
		return fmt.Errorf("写入共享库 schema 版本 %d: %w", SchemaVersion, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交共享库 schema 事务: %w", err)
	}
	return nil
}

// Package memnet 是"经验网"：给模型抓线头的记忆结构。
//
// 独立包：只依赖标准库与一个 *sql.DB（谁的库都行），不 import 本仓库其他包。
// 抽出来是为了将来能整块搬走（独立仓库 / 独立服务）——表、阈值、动作都是它自己的。
//
// 一句话：**树只有一层（类型），"枝"是读取时现算的。**
//
// 实测（bge-small-zh，20 条真经验）逼出了这个形状：
//   - 短句向量是各向异性的（全挤在一个窄锥里），原始余弦的"同义 / 无关"几乎重叠
//     （间隔 +0.068）；减掉语料均值后拉到 +0.216，才分得开。
//   - 持久化的第二层枝靠"成员均值"当中心点，会被异质成员带跑（质心融化），
//     一条"坑"下面能同时装着偏好、做法、事实——比单例更糟。
//
// 所以第二层不落库：写入时只挂到五个大类（可多挂），
// 线头（哪些经验属于同一个"枝"）在召回时按当前 query 现算，糊了下次可以重新聚。
//
// 三条不变量：
//  1. 节点没有名字——展示用成员首句；种子枝用预制句（anchor），名字不是事实。
//  2. 大类向量**锚在种子句上，永不随成员重算**。否则五个大类会彼此趋同，
//     最后什么都往最近的那个塞。
//  3. 遗忘曲线只作用在**条目**上：R = exp(-Δt/S)，S 随命中次数增长。
package memnet

import (
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store 是一张经验网。它有自己的一组表（mem_nodes / mem_entries / mem_links），
// 但句柄由调用方给——不关心那是不是会话库。
type Store struct {
	db   *sql.DB
	tune Tuning

	// 语料均值缓存。向量空间各向异性（全部挤在一个窄锥里），
	// 比较前减掉均值能把"同义 / 无关"的间隔拉开约三倍。
	// 派生数据：随语料变、可重算、不进库。
	mu        sync.Mutex
	mean      []float32
	meanDirty bool
}

// Open 建表、铺预制网，并返回一张可用的网。
// 幂等：重复调用不重复铺种子，也不动已有经验。
func Open(db *sql.DB, now time.Time) (*Store, error) {
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("建经验网表失败: %w", err)
	}
	s := &Store{db: db, tune: DefaultTuning()}
	if err := s.SeedNodes(now); err != nil {
		return nil, err
	}
	return s, nil
}

// Tuning 是这张网的手艺活：阈值定形状，全部可调（将来进设置中心）。
type Tuning struct {
	// MaxLinks：一条经验最多挂几个大类（1 = 单父树）。永远至少挂一个（argmax）。
	MaxLinks int
	// LinkL1：除 argmax 之外，还愿意挂的额外大类要够这个相似度。
	LinkL1 float64
	// MinScore：线头的最低分（cos × 留存度）。低于它的不进上下文——
	// 中心化之后无关条目的余弦接近 0，全给会把召回变成噪音。
	MinScore float64
	// NeighborSim：两条经验算"同一枝"的线（召回时聚簇、dig 时翻近邻）。
	NeighborSim float64
	NearDup     float64 // "同一件事"的判定线（写入时去重）
	BaseStable  float64 // 遗忘曲线：S₀（天）
	Growth      float64 // 遗忘曲线：每命中一次的强度倍数
	PruneRetain float64 // 软剪枝线（R 低于它标 pruned）
	// Center 是"比较前减掉语料均值"（各向异性修正）。
	// 实测：开着它，同义 / 无关的间隔从 +0.068 拉到 +0.216（约 3 倍）。
	// 关掉只在测试里用——那时向量是手写的，几何关系明确。
	Center bool
}

// DefaultTuning 是出厂值：形状参数按真模型实测标定（见 tests/notes_test.go 的注释）。
func DefaultTuning() Tuning {
	return Tuning{
		MaxLinks:    3,
		LinkL1:      0.15,
		MinScore:    0.05,
		NeighborSim: 0.30,
		NearDup:     0.62,
		BaseStable:  30,
		Growth:      2.0,
		PruneRetain: 0.15,
		Center:      true,
	}
}

// SetTuning 换一套参数（标定 / 设置下发）。
func (s *Store) SetTuning(t Tuning) {
	if t.MaxLinks <= 0 {
		t.MaxLinks = 1
	}
	s.tune = t
}

// Tuning 返回当前参数。
func (s *Store) Tuning() Tuning { return s.tune }

const (
	// flowBump 是一次实际使用（dig 到 / 再次观察）给链接加的粗细。
	flowBump = 0.08
	// linkCold 以下的链接视为干枯，可与条目断开。
	linkCold = 0.12
	// clusterSeed 是召回时最多拿多少条候选来聚簇（够铺满线头即可，不做全库 O(n²)）。
	clusterSeed = 40
)

// 预制网：只给第一层的 5 个"问题"，不给主题。
//
// 这五个是**正交于领域**的（不是"编程/生活/工作"这类领域切分）：
// "用 QQ 发图被限流"和"SQLite 迁移锁表"都落在「坑」里，不需要两个不同的种子去接。
//
// anchor 是**样本句**（不是"名字"）：它是这个大类向量的定义，永不随成员变化。
type seedNode struct {
	ID     string
	Anchor string
}

var seedNodes = []seedNode{
	{ID: "seed-pref", Anchor: "用户长期稳定的喜好与厌恶：对什么顺手、对什么反感、习惯怎么做"},
	{ID: "seed-fact", Anchor: "客观且相对稳定情况：环境、身份、账号、约定、位置"},
	{ID: "seed-how", Anchor: "把事做成的办法：步骤、工具、写法、技巧"},
	{ID: "seed-pitfall", Anchor: "失败与教训：出了什么问题、为什么、下次怎么绕开"},
	{ID: "seed-tradeoff", Anchor: "做过的选择与理由：为什么这样选、放弃了什么"},
}

const schema = `
CREATE TABLE IF NOT EXISTS mem_nodes (
  id             TEXT PRIMARY KEY,
  parent_id      TEXT NOT NULL DEFAULT '',
  level          INTEGER NOT NULL,
  anchor         TEXT NOT NULL,
  entries        INTEGER NOT NULL DEFAULT 0,
  centroid       BLOB,
  centroid_model TEXT NOT NULL DEFAULT '',
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS mem_entries (
  id              TEXT PRIMARY KEY,
  content         TEXT NOT NULL,
  conversation_id TEXT NOT NULL DEFAULT '',
  task_id         TEXT NOT NULL DEFAULT '',
  source          TEXT NOT NULL DEFAULT 'agent',
  durability      TEXT NOT NULL DEFAULT 'durable',
  hits            INTEGER NOT NULL DEFAULT 1,
  status          TEXT NOT NULL DEFAULT 'active',
  last_hit_at     TEXT NOT NULL,
  created_at      TEXT NOT NULL,
  embedding       BLOB,
  embedding_model TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_mem_entries_status ON mem_entries(status);

CREATE TABLE IF NOT EXISTS mem_links (
  entry_id     TEXT NOT NULL,
  node_id      TEXT NOT NULL,
  conductance  REAL NOT NULL,
  last_flow_at TEXT NOT NULL,
  PRIMARY KEY (entry_id, node_id)
);
CREATE INDEX IF NOT EXISTS idx_mem_links_node ON mem_links(node_id);
`

// Note 是一条经验。
type Note struct {
	ID             string
	Content        string
	ConversationID string
	TaskID         string
	Source         string
	Durability     string
	Hits           int
	Status         string
	LastHitAt      time.Time
	CreatedAt      time.Time
	// NodeIDs 是它挂靠的大类（按 conductance 降序，第一个是主枝）。
	NodeIDs []string

	// vec 是这个条目的向量（内部用，不导出）。
	vec []float32
}

// Node 是一个大类枝。没有名字：Anchor 是种子句，只在空枝时展示。
type Node struct {
	ID      string
	Level   int
	Anchor  string
	Entries int
}

// LineHead 是给模型看的"线头"：一组被当前 query 拉出来的同类经验。
//
// 它不是持久化的枝——是**读取时现算**的簇：糊了下次可以重新聚，不会永久长歪。
type LineHead struct {
	// NodeID 是这组经验的主大类（派生，用于图省事地给个"类型"）。
	NodeID  string
	Text    string  // 代表句（簇里最相关那条的首句）
	Count   int     // 这一簇有几条
	Score   float64 // 代表句的相关度 × 留存度
	TaskID  string
	Members []string // 簇内条目的 id（供模型决定要不要 dig）
}

// AttachInput 是写一条经验的输入。
type AttachInput struct {
	Content        string
	ConversationID string
	TaskID         string
	Source         string // agent | user
	Durability     string // core | durable | transient（transient 不进网）
}

// AttachResult 说明这次写入是新建还是并进了已有条目。
type AttachResult struct {
	EntryID   string
	NodeIDs   []string
	Duplicate bool // 近重复：加了命中次数，没造新条目
	Skipped   bool // transient：只属于当前任务，不进网
}

// ---------- 遗忘曲线 ----------

// Stability 返回条目当前的记忆强度（天）：S₀ × growth^(hits-1)，core 视为不衰减。
func (s *Store) Stability(hits int, durability string) float64 {
	if durability == "core" {
		return math.Inf(1)
	}
	if hits < 1 {
		hits = 1
	}
	return s.tune.BaseStable * math.Pow(s.tune.Growth, float64(hits-1))
}

// Retention 返回此刻还记得的概率：R = exp(-Δt / S)。
func (s *Store) Retention(hits int, durability string, lastHit, now time.Time) float64 {
	sd := s.Stability(hits, durability)
	if math.IsInf(sd, 1) {
		return 1
	}
	days := now.Sub(lastHit).Hours() / 24
	if days < 0 {
		days = 0
	}
	return math.Exp(-days / sd)
}

// ---------- 初始化与种子 ----------

// SeedNodes 铺预制网（幂等：已存在的枝不动）。
func (s *Store) SeedNodes(now time.Time) error {
	ts := now.Format(time.RFC3339Nano)
	for _, n := range seedNodes {
		if _, err := s.db.Exec(
			`INSERT OR IGNORE INTO mem_nodes(id, parent_id, level, anchor, created_at, updated_at)
			 VALUES(?,?,1,?,?,?)`, n.ID, "", n.Anchor, ts, ts); err != nil {
			return fmt.Errorf("铺预制网失败(%s): %w", n.ID, err)
		}
	}
	return nil
}

// PendingCentroids 返回还没有向量的枝（供上层在 embedding 可用时补齐定义向量）。
func (s *Store) PendingCentroids() ([]Node, error) {
	rows, err := s.db.Query(`SELECT id, level, anchor FROM mem_nodes WHERE centroid IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.Level, &n.Anchor); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// SetNodeCentroid 写入枝的定义向量。**只在建种子时调**：
// 大类向量锚在种子句上，不随成员重算（否则五个大类会趋同成一个）。
func (s *Store) SetNodeCentroid(id string, vec []float32, model string) error {
	_, err := s.db.Exec(`UPDATE mem_nodes SET centroid=?, centroid_model=?, updated_at=? WHERE id=?`,
		encodeVec(vec), model, time.Now().Format(time.RFC3339Nano), id)
	s.invalidateMean()
	return err
}

// Nodes 返回五个大类与各自的成员数（成员数按链接现算，不落盘）。
func (s *Store) Nodes() ([]Node, error) {
	rows, err := s.db.Query(
		`SELECT n.id, n.level, n.anchor,
		        (SELECT COUNT(DISTINCT l.entry_id) FROM mem_links l
		           JOIN mem_entries e ON e.id=l.entry_id
		          WHERE l.node_id=n.id AND e.status='active')
		   FROM mem_nodes n ORDER BY n.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.Level, &n.Anchor, &n.Entries); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ---------- 写入 ----------

// Attach 写一条经验：先查近重复（同一件事只留一条，加命中次数），
// 否则按向量挂到大类上（argmax 必挂，其余够近的加挂）。
func (s *Store) Attach(now time.Time, in AttachInput, vec []float32, model string) (AttachResult, error) {
	content := strings.TrimSpace(in.Content)
	if content == "" {
		return AttachResult{}, errors.New("经验内容为空")
	}
	if in.Durability == "transient" {
		// 一次性的东西属于当前任务上下文，不属于经验。
		return AttachResult{Skipped: true}, nil
	}
	if in.Source == "" {
		in.Source = "agent"
	}
	if in.Durability == "" {
		in.Durability = "durable"
	}
	ts := now.Format(time.RFC3339Nano)

	if id, ok, err := s.findNearDuplicate(vec); err != nil {
		return AttachResult{}, err
	} else if ok {
		if err := s.reinforce(id, vec, now); err != nil {
			return AttachResult{}, err
		}
		nodes, err := s.nodeIDsOf(id)
		return AttachResult{EntryID: id, NodeIDs: nodes, Duplicate: true}, err
	}

	// 先算挂靠再插库：大类向量还没补齐时要当场失败，
	// 不能先写进一条"没有归属"的经验（半条经验比没记更糟）。
	links, err := s.chooseNodes(vec)
	if err != nil {
		return AttachResult{}, err
	}

	entryID := newNoteID(now)
	if _, err := s.db.Exec(
		`INSERT INTO mem_entries(id, content, conversation_id, task_id, source, durability, hits, status, last_hit_at, created_at, embedding, embedding_model)
		 VALUES(?,?,?,?,?,?,1,'active',?,?,?,?)`,
		entryID, content, in.ConversationID, in.TaskID, in.Source, in.Durability, ts, ts, encodeVec(vec), model); err != nil {
		return AttachResult{}, err
	}
	s.invalidateMean()

	ids := make([]string, 0, len(links))
	for _, l := range links {
		if _, err := s.db.Exec(
			`INSERT OR REPLACE INTO mem_links(entry_id, node_id, conductance, last_flow_at) VALUES(?,?,?,?)`,
			entryID, l.id, l.conductance, ts); err != nil {
			return AttachResult{}, err
		}
		ids = append(ids, l.id)
	}
	return AttachResult{EntryID: entryID, NodeIDs: ids}, nil
}

type nodeChoice struct {
	id          string
	conductance float64
}

// chooseNodes 选大类：argmax 无条件挂上，其余够 LinkL1 的加挂（这就是"网"）。
// 大类**不会**因为够不着而新建——五个大类覆盖全部经验类型，改分类由人加种子。
func (s *Store) chooseNodes(vec []float32) ([]nodeChoice, error) {
	centroids, err := s.centroids()
	if err != nil {
		return nil, err
	}
	mean := s.meanVec()
	vin := centerVec(vec, mean)
	var all []nodeChoice
	for id, c := range centroids {
		if len(c) == 0 {
			continue
		}
		// 中心化之后余弦可以是负数（"离得很远"），照样要参与排序：
		// argmax 是必须给出的家，不能因为全为负就没有去处。
		all = append(all, nodeChoice{id: id, conductance: cosine(vin, centerVec(c, mean))})
	}
	if len(all) == 0 {
		return nil, errors.New("没有可用的大类向量（种子尚未补齐）")
	}
	sort.Slice(all, func(i, j int) bool { return all[i].conductance > all[j].conductance })

	picks := []nodeChoice{all[0]}
	for _, c := range all[1:] {
		if len(picks) >= s.tune.MaxLinks {
			break
		}
		if c.conductance >= s.tune.LinkL1 {
			picks = append(picks, c)
		}
	}
	return picks, nil
}

// findNearDuplicate 找"同一件事"：向量够近就算。
func (s *Store) findNearDuplicate(vec []float32) (string, bool, error) {
	if len(vec) == 0 {
		return "", false, nil // 没有向量就不做近重复合并：宁可多一条，也不要错并
	}
	mean := s.meanVec()
	vin := centerVec(vec, mean)
	rows, err := s.db.Query(
		`SELECT id, embedding FROM mem_entries
		  WHERE status='active' AND durability <> 'transient' AND embedding IS NOT NULL`)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	best, bestScore := "", 0.0
	for rows.Next() {
		var (
			id   string
			blob []byte
		)
		if err := rows.Scan(&id, &blob); err != nil {
			return "", false, err
		}
		if sc := cosine(vin, centerVec(decodeVec(blob), mean)); sc > bestScore {
			best, bestScore = id, sc
		}
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	return best, bestScore >= s.tune.NearDup, nil
}

// reinforce 把已有条目"又遇到了一次"：命中 +1、刷新时间、主枝加粗。
func (s *Store) reinforce(id string, vec []float32, now time.Time) error {
	ts := now.Format(time.RFC3339Nano)
	if len(vec) > 0 {
		if _, err := s.db.Exec(`UPDATE mem_entries SET hits=hits+1, last_hit_at=?, embedding=? WHERE id=?`,
			ts, encodeVec(vec), id); err != nil {
			return err
		}
	} else if _, err := s.db.Exec(`UPDATE mem_entries SET hits=hits+1, last_hit_at=? WHERE id=?`, ts, id); err != nil {
		return err
	}
	s.invalidateMean()
	nodes, err := s.nodeIDsOf(id)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if err := s.Flow(id, n, now); err != nil {
			return err
		}
	}
	return nil
}

// Flow 记一次"这条经验从某个大类上被翻到"：加粗链接。
// 只在实际用到时调用——召回本身不加粗，否则会出现"越常想起越忘不掉"的回音室。
func (s *Store) Flow(entryID, nodeID string, now time.Time) error {
	_, err := s.db.Exec(
		`UPDATE mem_links SET conductance=MIN(1.0, conductance+?), last_flow_at=? WHERE entry_id=? AND node_id=?`,
		flowBump, now.Format(time.RFC3339Nano), entryID, nodeID)
	return err
}

// ---------- 读取 ----------

// scored 是召回打分用的一次性记录。
type scored struct {
	id      string
	content string
	taskID  string
	nodeID  string
	vec     []float32
	score   float64
	last    time.Time
}

// LineHeads 是"条目优先"的召回：**先在全库条目里找**（这一步不会漏跨大类挂靠的经验），
// 再把 top 候选按当前 query 现算成簇——每簇一个线头。
//
// 簇是派生的：这次聚糊了，下次 query 换了可以重新聚，不会永久长歪。
func (s *Store) LineHeads(qvec []float32, now time.Time, limit int) ([]LineHead, error) {
	if limit <= 0 {
		limit = 6
	}
	cands, err := s.scoredEntries(qvec, now)
	if err != nil {
		return nil, err
	}
	// 低于最低分的直接丢掉：宁缺勿滥。
	kept := cands[:0]
	for _, c := range cands {
		if c.score >= s.tune.MinScore {
			kept = append(kept, c)
		}
	}
	cands = kept

	if len(cands) > clusterSeed {
		cands = cands[:clusterSeed]
	}

	used := make([]bool, len(cands))
	var heads []LineHead
	for i, c := range cands {
		if used[i] {
			continue
		}
		used[i] = true
		head := LineHead{
			NodeID:  c.nodeID,
			Text:    sentence(c.content),
			Count:   1,
			Score:   c.score,
			TaskID:  c.taskID,
			Members: []string{c.id},
		}
		for j := i + 1; j < len(cands); j++ {
			if used[j] {
				continue
			}
			if len(c.vec) > 0 && len(cands[j].vec) > 0 && cosine(c.vec, cands[j].vec) >= s.tune.NeighborSim {
				used[j] = true
				head.Count++
				head.Members = append(head.Members, cands[j].id)
			}
		}
		heads = append(heads, head)
		if len(heads) >= limit {
			break
		}
	}
	return heads, nil
}

// scoredEntries 把 active 条目按"相关度 × 留存度"排好（qvec 为空时只看留存度）。
func (s *Store) scoredEntries(qvec []float32, now time.Time) ([]scored, error) {
	mean := s.meanVec()
	qv := centerVec(qvec, mean)
	rows, err := s.db.Query(
		`SELECT e.id, e.content, e.task_id, e.hits, e.durability, e.last_hit_at, e.embedding,
		        COALESCE((SELECT l.node_id FROM mem_links l WHERE l.entry_id=e.id
		                  ORDER BY l.conductance DESC LIMIT 1), '')
		   FROM mem_entries e
		  WHERE e.status='active' AND e.durability <> 'transient'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scored
	for rows.Next() {
		var (
			c                     scored
			hits                  int
			durability, lastHitAt string
			blob                  []byte
		)
		if err := rows.Scan(&c.id, &c.content, &c.taskID, &hits, &durability, &lastHitAt, &blob, &c.nodeID); err != nil {
			return nil, err
		}
		c.last, _ = time.Parse(time.RFC3339Nano, lastHitAt)
		c.vec = centerVec(decodeVec(blob), mean)
		c.score = s.Retention(hits, durability, c.last, now)
		if len(qv) > 0 && len(c.vec) > 0 {
			sim := cosine(qv, c.vec)
			if sim < 0 {
				sim = 0
			}
			c.score = sim * c.score
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].score > out[j].score })
	return out, nil
}

// DigEntry 以一条经验为中心翻近邻：**全库按相似度找，不要求同大类**。
//
// 曾经要求"同大类"，结果自断线索：两条讲同一件事的经验被粗糙的五个桶分到了
// 不同桶（桶之间的余弦只有 0.1~0.2，本来就不可靠），于是永远翻不到彼此。
// 条目优先的原则同样适用在这里——桶只是标签，不是门槛。
func (s *Store) DigEntry(entryID string, now time.Time, limit int) (Note, []Note, int, error) {
	if limit <= 0 {
		limit = 20
	}
	center, err := s.note(entryID)
	if err != nil {
		return Note{}, nil, 0, err
	}
	mean := s.meanVec()
	centerCentered := centerVec(center.vec, mean)
	rows, err := s.db.Query(
		`SELECT e.id, e.content, e.conversation_id, e.task_id, e.source, e.durability,
		        e.hits, e.status, e.last_hit_at, e.created_at, e.embedding
		   FROM mem_entries e
		  WHERE e.status='active' AND e.id <> ? AND e.embedding IS NOT NULL`,
		entryID)
	if err != nil {
		return Note{}, nil, 0, err
	}
	defer rows.Close()

	type cand struct {
		note Note
		sim  float64
	}
	var cands []cand
	for rows.Next() {
		var (
			n                    Note
			lastHitAt, createdAt string
			blob                 []byte
		)
		if err := rows.Scan(&n.ID, &n.Content, &n.ConversationID, &n.TaskID, &n.Source, &n.Durability,
			&n.Hits, &n.Status, &lastHitAt, &createdAt, &blob); err != nil {
			return Note{}, nil, 0, err
		}
		n.LastHitAt, _ = time.Parse(time.RFC3339Nano, lastHitAt)
		n.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		sim := 0.0
		if len(centerCentered) > 0 && len(blob) > 0 {
			sim = cosine(centerCentered, centerVec(decodeVec(blob), mean))
		}
		if len(centerCentered) == 0 || sim >= s.tune.NeighborSim {
			cands = append(cands, cand{note: n, sim: sim})
		}
	}
	if err := rows.Err(); err != nil {
		return Note{}, nil, 0, err
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].sim != cands[j].sim {
			return cands[i].sim > cands[j].sim
		}
		return cands[i].note.LastHitAt.After(cands[j].note.LastHitAt)
	})
	out := make([]Note, 0, limit)
	for _, c := range cands {
		if len(out) >= limit {
			break
		}
		out = append(out, c.note)
	}
	return center, out, len(cands), nil
}

// DigNode 展开一个大类（门户浏览用）：该类下所有 active 条目，按留存度排。
func (s *Store) DigNode(nodeID string, now time.Time, limit int) (Node, []Note, int, error) {
	if limit <= 0 {
		limit = 20
	}
	node, err := s.category(nodeID)
	if err != nil {
		return Node{}, nil, 0, err
	}
	rows, err := s.db.Query(
		`SELECT e.id, e.content, e.conversation_id, e.task_id, e.source, e.durability,
		        e.hits, e.status, e.last_hit_at, e.created_at
		   FROM mem_links l JOIN mem_entries e ON e.id = l.entry_id
		  WHERE l.node_id=? AND e.status='active'
		  ORDER BY e.hits DESC, e.last_hit_at DESC`,
		nodeID)
	if err != nil {
		return Node{}, nil, 0, err
	}
	defer rows.Close()
	var (
		out   []Note
		total int
	)
	for rows.Next() {
		var (
			n                    Note
			lastHitAt, createdAt string
		)
		if err := rows.Scan(&n.ID, &n.Content, &n.ConversationID, &n.TaskID, &n.Source, &n.Durability,
			&n.Hits, &n.Status, &lastHitAt, &createdAt); err != nil {
			return Node{}, nil, 0, err
		}
		n.LastHitAt, _ = time.Parse(time.RFC3339Nano, lastHitAt)
		n.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		total++
		if len(out) < limit {
			out = append(out, n)
		}
	}
	return node, out, total, rows.Err()
}

// ---------- 剪枝 ----------

// PruneResult 汇总一次维护的结果。
type PruneResult struct {
	Entries int // 标为 pruned 的条目
	Links   int // 干枯断开的链接
}

// Prune 是软剪枝：只标状态、只断干枯的线，不删行、不删向量
// （忘掉之后再学一遍比第一次快，S 要留着）。
func (s *Store) Prune(now time.Time) (PruneResult, error) {
	var res PruneResult

	type pending struct {
		id    string
		faded bool
	}
	var list []pending
	rows, err := s.db.Query(`SELECT id, hits, durability, last_hit_at FROM mem_entries WHERE status='active'`)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var id, durability, lastHitAt string
		var hits int
		if err := rows.Scan(&id, &hits, &durability, &lastHitAt); err != nil {
			rows.Close()
			return res, err
		}
		last, _ := time.Parse(time.RFC3339Nano, lastHitAt)
		list = append(list, pending{id: id, faded: s.Retention(hits, durability, last, now) < s.tune.PruneRetain})
	}
	rows.Close()

	for _, p := range list {
		if !p.faded {
			continue
		}
		if _, err := s.db.Exec(`UPDATE mem_entries SET status='pruned' WHERE id=?`, p.id); err != nil {
			return res, err
		}
		res.Entries++
	}

	// 干枯的线断掉；条目只要还有一条线就还活着。
	r, err := s.db.Exec(
		`DELETE FROM mem_links WHERE conductance < ? AND last_flow_at < ?`,
		linkCold, now.Add(-30*24*time.Hour).Format(time.RFC3339Nano))
	if err != nil {
		return res, err
	}
	if n, _ := r.RowsAffected(); n > 0 {
		res.Links = int(n)
	}
	if _, err := s.db.Exec(
		`UPDATE mem_entries SET status='pruned'
		  WHERE status='active' AND id NOT IN (SELECT DISTINCT entry_id FROM mem_links)`); err != nil {
		return res, err
	}
	s.invalidateMean()
	return res, nil
}

// ---------- 内部 ----------

func (s *Store) note(id string) (Note, error) {
	var (
		n                    Note
		lastHitAt, createdAt string
		blob                 []byte
	)
	err := s.db.QueryRow(
		`SELECT id, content, conversation_id, task_id, source, durability, hits, status, last_hit_at, created_at, embedding
		   FROM mem_entries WHERE id=?`, id).
		Scan(&n.ID, &n.Content, &n.ConversationID, &n.TaskID, &n.Source, &n.Durability,
			&n.Hits, &n.Status, &lastHitAt, &createdAt, &blob)
	if err != nil {
		return Note{}, err
	}
	n.LastHitAt, _ = time.Parse(time.RFC3339Nano, lastHitAt)
	n.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	n.vec = decodeVec(blob)
	return n, nil
}

func (s *Store) category(id string) (Node, error) {
	var n Node
	err := s.db.QueryRow(
		`SELECT n.id, n.level, n.anchor,
		        (SELECT COUNT(DISTINCT l.entry_id) FROM mem_links l
		           JOIN mem_entries e ON e.id=l.entry_id
		          WHERE l.node_id=n.id AND e.status='active')
		   FROM mem_nodes n WHERE n.id=?`, id).Scan(&n.ID, &n.Level, &n.Anchor, &n.Entries)
	return n, err
}

func (s *Store) centroids() (map[string][]float32, error) {
	rows, err := s.db.Query(`SELECT id, centroid FROM mem_nodes WHERE centroid IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]float32{}
	for rows.Next() {
		var id string
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, err
		}
		out[id] = decodeVec(blob)
	}
	return out, rows.Err()
}

func (s *Store) nodeIDsOf(entryID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT node_id FROM mem_links WHERE entry_id=? ORDER BY conductance DESC`, entryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---------- 各向异性修正（语料均值） ----------

// meanVec 返回语料均值：所有 active 条目向量 + 种子向量的平均。
// 减掉它再算余弦，是这套模型能分清"同一件事"的前提（见文件头注释）。
// 派生数据：随语料变化，写/删后失效重算。
func (s *Store) meanVec() []float32 {
	if !s.tune.Center {
		return nil
	}
	s.mu.Lock()
	if !s.meanDirty && s.mean != nil {
		m := s.mean
		s.mu.Unlock()
		return m
	}
	s.mu.Unlock()

	rows, err := s.db.Query(
		`SELECT embedding FROM mem_entries WHERE status='active' AND embedding IS NOT NULL
		 UNION ALL SELECT centroid FROM mem_nodes WHERE centroid IS NOT NULL`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var sum []float64
	var n int
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			return nil
		}
		v := decodeVec(blob)
		if len(v) == 0 {
			continue
		}
		if sum == nil {
			sum = make([]float64, len(v))
		}
		if len(v) != len(sum) {
			continue
		}
		for i := range v {
			sum[i] += float64(v[i])
		}
		n++
	}
	if n == 0 {
		return nil
	}
	mean := make([]float32, len(sum))
	for i := range sum {
		mean[i] = float32(sum[i] / float64(n))
	}
	s.mu.Lock()
	s.mean, s.meanDirty = mean, false
	s.mu.Unlock()
	return mean
}

// invalidateMean 让均值缓存失效（新增 / 剪枝 / 改种子向量之后调）。
func (s *Store) invalidateMean() {
	s.mu.Lock()
	s.meanDirty = true
	s.mu.Unlock()
}

// centerVec 减去语料均值并归一化。mean 为空时退回归一化原向量。
func centerVec(v, mean []float32) []float32 {
	if len(v) == 0 {
		return v
	}
	if len(mean) != len(v) {
		return unit(v)
	}
	out := make([]float32, len(v))
	for i := range v {
		out[i] = v[i] - mean[i]
	}
	return unit(out)
}

func unit(v []float32) []float32 {
	var n float64
	for _, f := range v {
		n += float64(f) * float64(f)
	}
	if n == 0 {
		return v
	}
	n = math.Sqrt(n)
	for i := range v {
		v[i] = float32(float64(v[i]) / n)
	}
	return v
}

func newNoteID(now time.Time) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("n%s-%s", now.Format("20060102T150405"), hex.EncodeToString(b[:]))
}

// sentence 取首句并截断——线头要一眼看完。
func sentence(s string) string {
	s = strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	r := []rune(s)
	if len(r) > 48 {
		return string(r[:48]) + "…"
	}
	return s
}

func encodeVec(v []float32) []byte {
	if len(v) == 0 {
		return nil
	}
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

func decodeVec(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
}

func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

var _ = sql.ErrNoRows

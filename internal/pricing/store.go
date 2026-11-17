// store.go 牌价表的热加载：data/prices.json（手改）→ 内存只读快照。
//
// 语义（与 internal/upstream/model.json 同一套「种子 + 旁挂文件 + 静默降级」思路）：
//   - 文件不存在：用内置种子，Source="seed"，不报错（这是支持形态，不是故障）；
//   - 文件存在且合法：种子 ∪ 文件覆盖，Source="seed+file"，快照日期取文件里的 updated；
//   - 文件坏 / 币种或单位不符：**保留上一版成功的表**，把错误挂在 Meta().Error 上，
//     面板照常出数（错的只是"新价没生效"，不会让整页金额归零）；
//   - 文件被删掉：下一次检查回到种子。
//
// 热更新靠 mtime+size 脏检查：面板每次拉用量都会 Current() 一次，os.Stat 的代价
// 可以忽略；改完 prices.json 刷新面板即生效，不需要重启，也不需要动 config.json。
package pricing

import (
	"log"
	"os"
	"sync"
	"time"
)

// Store 并发安全的牌价表持有者（读侧拿快照指针，写侧整体替换）。
type Store struct {
	path string

	mu    sync.Mutex
	table *Table
	mod   time.Time
	size  int64
	seen  bool   // 是否已成功读到过文件（用于判断"文件被删掉"）
	err   string // 最近一次加载错误（挂在 Meta 上透出）
}

// NewStore 创建并立即尝试加载一次。path 为空 = 只用种子表（测试/裸用）。
func NewStore(path string) *Store {
	s := &Store{path: path, table: Seed()}
	s.refresh(true)
	return s
}

// Path 返回牌价文件路径（面板展示用）。
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Current 返回当前生效的牌价表（nil 接收者返回 nil，调用方据此退化为"未启用"）。
//
// 每次调用都做一次 mtime+size 脏检查，文件改动即时生效。
func (s *Store) Current() *Table {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path != "" {
		s.refresh(false)
	}
	return s.table
}

// Meta 返回当前牌价表元信息。
func (s *Store) Meta() Meta {
	t := s.Current()
	if t == nil {
		return Meta{}
	}
	return t.Meta()
}

// refresh 检查并重载。调用方必须持锁。force=true 表示首次加载（无论 mtime 都读一次）。
func (s *Store) refresh(force bool) {
	fi, err := os.Stat(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			if s.seen {
				log.Printf("[pricing] 牌价文件 %s 已不存在，回落到内置种子牌价", s.path)
				s.seen = false
				s.mod = time.Time{}
				s.size = 0
			}
			if s.err != "" || s.table == nil || s.table.Meta().Source != "seed" {
				s.err = ""
				s.table = Seed()
			}
			return
		}
		s.setErr("stat: " + err.Error())
		return
	}
	if !force && s.seen && fi.ModTime().Equal(s.mod) && fi.Size() == s.size {
		return
	}
	raw, readErr := os.ReadFile(s.path)
	s.mod, s.size, s.seen = fi.ModTime(), fi.Size(), true
	if readErr != nil {
		s.setErr("读取失败: " + readErr.Error())
		return
	}
	rows, updated, warns, parseErr := ParseFile(raw)
	if parseErr != nil {
		s.setErr(parseErr.Error())
		log.Printf("[pricing] 牌价文件 %s 解析失败，沿用上一版：%v", s.path, parseErr)
		return
	}
	for _, w := range warns {
		log.Printf("[pricing] %s：%s", s.path, w)
	}
	s.err = ""
	s.table = NewTable(rows, Meta{Source: "seed+file", Updated: updated})
	m := s.table.Meta()
	log.Printf("[pricing] 牌价文件 %s 已加载：%d 个模型（其中 %d 条来自文件覆盖），快照 %s",
		s.path, m.Models, len(rows), m.Updated)
}

// setErr 记录错误并把它挂到当前表上（数据不变，只是展示层可见）。
func (s *Store) setErr(msg string) {
	s.err = msg
	if s.table == nil {
		s.table = Seed()
	}
	s.table = s.table.withError(msg)
}

// withError 返回挂了加载错误的浅拷贝（models 共享，只读）。
func (t *Table) withError(msg string) *Table {
	if t == nil {
		t = Seed()
	}
	m := t.meta
	m.Error = msg
	return &Table{models: t.models, meta: m}
}

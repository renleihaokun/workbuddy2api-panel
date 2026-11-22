// kind_test.go 事件注册表：完整性、分组索引、堆叠柱折算。
package channelstat

import "testing"

// 注册表必须覆盖全部已知事件、字段齐全、顺序唯一，且不再包含死条目。
func TestRegistryComplete(t *testing.T) {
	specs := Kinds()
	if len(specs) != 18 {
		t.Fatalf("registry size=%d want 18（新增事件要同步改本测试与 README）", len(specs))
	}
	seen := make(map[Kind]bool, len(specs))
	for i, s := range specs {
		if s.Kind == "" {
			t.Fatalf("spec[%d] has empty Kind", i)
		}
		if seen[s.Kind] {
			t.Errorf("duplicate Kind %q", s.Kind)
		}
		seen[s.Kind] = true
		if s.Label == "" {
			t.Errorf("%s: empty Label", s.Kind)
		}
		if s.Hint == "" {
			t.Errorf("%s: empty Hint（事件表靠它解释「是什么/该怎么办」）", s.Kind)
		}
		switch s.Group {
		case GroupRefusal, GroupThrottle, GroupStream, GroupOther, GroupOffchart:
		default:
			t.Errorf("%s: invalid Group %q", s.Kind, s.Group)
		}
		switch s.Unit {
		case UnitRequest, UnitWork:
		default:
			t.Errorf("%s: invalid Unit %q", s.Kind, s.Unit)
		}
		if got := Order(s.Kind); got != i {
			t.Errorf("%s: Order=%d want %d", s.Kind, got, i)
		}
		if got := Label(s.Kind); got != s.Label {
			t.Errorf("%s: Label=%q want %q", s.Kind, got, s.Label)
		}
	}
	if seen[Kind("model_rate_limit")] {
		t.Error("model_rate_limit 是死条目（6004/6000 已归 soft_rate），不应再登记")
	}
}

// 堆叠柱元数据固定为 refusal/throttle/stream/other，且每组至少有一个事件。
func TestChartGroups(t *testing.T) {
	gs := ChartGroups()
	want := []Group{GroupRefusal, GroupThrottle, GroupStream, GroupOther}
	if len(gs) != len(want) {
		t.Fatalf("chart groups=%d want %d", len(gs), len(want))
	}
	for i, g := range gs {
		if g.Key != want[i] {
			t.Errorf("group[%d]=%q want %q", i, g.Key, want[i])
		}
		if g.Label == "" {
			t.Errorf("group %q: empty label", g.Key)
		}
		found := false
		for _, s := range Kinds() {
			if s.Group == g.Key {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("group %q has no registered kind", g.Key)
		}
	}
}

// 未登记 kind 不丢：GroupOf 落 other，Label 原样返回，Order 为 -1。
func TestGroupOfUnknownKind(t *testing.T) {
	if got := GroupOf(KindChannelRefused); got != GroupRefusal {
		t.Errorf("channel_refused group=%q want refusal", got)
	}
	if got := GroupOf(KindRoleNormalized); got != GroupOffchart {
		t.Errorf("role_normalized group=%q want offchart", got)
	}
	unknown := Kind("weird_kind")
	if got := GroupOf(unknown); got != GroupOther {
		t.Errorf("unknown group=%q want other", got)
	}
	if got := Label(unknown); got != "weird_kind" {
		t.Errorf("unknown label=%q want raw kind", got)
	}
	if got := Order(unknown); got != -1 {
		t.Errorf("unknown order=%d want -1", got)
	}
}

// groupsOf：other 用总数倒推，未登记与显式 other 都落进去，offchart 被减掉。
func TestGroupsOf(t *testing.T) {
	kinds := map[string]int64{
		"channel_refused": 3,
		"soft_rate":       1,
		"stream_error":    2,
		"role_normalized": 10, // offchart
		"cache_miss":      4,  // 显式 other
		"weird_kind":      5,  // 未登记 → other
	}
	got := groupsOf(kinds, 25)
	want := map[string]int64{
		"refusal": 3, "throttle": 1, "stream": 2, "other": 9, // 25-6-10
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("groups[%s]=%d want %d (all=%v)", k, got[k], v, got)
		}
	}
	if len(got) != 4 {
		t.Errorf("groups keys=%v want exactly 4 charted groups", got)
	}

	// 空桶也要给四组零值（前端按 key 取值，不判 nil）。
	empty := groupsOf(nil, 0)
	if len(empty) != 4 {
		t.Fatalf("empty groups=%v want 4 zero entries", empty)
	}
	for k, v := range empty {
		if v != 0 {
			t.Errorf("empty groups[%s]=%d want 0", k, v)
		}
	}

	// total 小于已列之和（脏数据）：other 不能为负。
	neg := groupsOf(map[string]int64{"channel_refused": 10, "role_normalized": 5}, 3)
	if neg["other"] < 0 {
		t.Errorf("other=%d must not be negative", neg["other"])
	}
}

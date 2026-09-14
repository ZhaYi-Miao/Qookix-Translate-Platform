package main

import (
	"strings"
	"testing"
	"time"
)

// resetAdvBuckets 把采样器状态清干净，避免测试互相污染。
func resetAdvBuckets(seeded bool) {
	advMu.Lock()
	advBuckets = map[string]map[int64]*advBucket{}
	advHeartbeat = map[int64]bool{}
	advLastCum = map[string][3]int64{}
	advSeeded = seeded
	advMu.Unlock()
}

// TestAdvWindowSum 窗口求和 + 覆盖率 + 上一窗口对比（趋势）是本次改造的核心，
// 模型的所有判断都建立在这几个数上，算错了后面全白搭。
func TestAdvWindowSum(t *testing.T) {
	resetAdvBuckets(true)

	// 对齐到分钟边界，让 [now-10m, now) 正好覆盖 10 个分钟桶
	now := time.Now().Truncate(time.Minute).Add(time.Minute)

	put := func(account string, minutesAgo int, calls, failed, timing int64, errs map[string]int) {
		min := now.Add(-time.Duration(minutesAgo)*time.Minute).Unix() / 60
		advMu.Lock()
		advHeartbeat[min] = true
		advBucketLocked(account, min).Add([3]int64{calls, failed, timing}, errs)
		advMu.Unlock()
	}

	// 本窗口（最近 10 分钟）：每分钟 2 次调用 / 1 次失败 / 200ms，第 3 分钟有 4 次排队超时
	for i := 1; i <= 10; i++ {
		var errs map[string]int
		if i == 3 {
			errs = map[string]int{"rpm_queue_timeout": 4}
		}
		put("providerA", i, 2, 1, 200, errs)
	}
	// 上一窗口（前 10 分钟）：只有前 5 分钟有量，量也更小
	for i := 11; i <= 15; i++ {
		put("providerA", i, 1, 0, 300, nil)
	}

	from := now.Add(-10 * time.Minute)
	prev := from.Add(-10 * time.Minute)

	calls, failed, timing, errs, minutes := advWindowStat("providerA", from, now)
	if calls != 20 || failed != 10 || timing != 2000 {
		t.Errorf("本窗口求和不对: calls=%d failed=%d timing=%d（期望 20/10/2000）", calls, failed, timing)
	}
	if errs["rpm_queue_timeout"] != 4 {
		t.Errorf("本窗口排队超时 = %d，期望 4", errs["rpm_queue_timeout"])
	}
	if minutes != 10 {
		t.Errorf("本窗口采样分钟数 = %d，期望 10", minutes)
	}
	if avg := advDiv(timing, calls); avg != 100 {
		t.Errorf("窗口平均延迟 = %d，期望 100", avg)
	}

	pCalls, _, _, _, _ := advWindowStat("providerA", prev, from)
	if pCalls != 5 {
		t.Errorf("上一窗口调用数 = %d，期望 5", pCalls)
	}

	// 趋势：20 vs 5 应判"上升"
	if got := advTrend(calls, pCalls, "次"); !strings.Contains(got, "上升") {
		t.Errorf("趋势判定 = %q，期望判为上升", got)
	}
	// 持平 / 归零 的边界
	if got := advTrend(10, 10, "次"); !strings.Contains(got, "持平") {
		t.Errorf("相等时应判持平，得到 %q", got)
	}
	if got := advTrend(0, 7, "次"); !strings.Contains(got, "降到 0") {
		t.Errorf("归零时应说明降到 0，得到 %q", got)
	}

	// 每百次调用口径：4 次排队 / 20 次调用 = 20
	if got := advPer100(errs["rpm_queue_timeout"], calls); got != 20 {
		t.Errorf("queue_per_100c = %v，期望 20", got)
	}

	// 覆盖率：本窗口 10 分钟都有采样 = 100%；上一窗口只有 5 分钟有采样 = 50%
	if cov := advSampleCoverage(from, now); cov != 1 {
		t.Errorf("本窗口覆盖率 = %v，期望 1", cov)
	}
	if cov := advSampleCoverage(prev, from); cov != 0.5 {
		t.Errorf("上一窗口覆盖率 = %v，期望 0.5", cov)
	}

	// 另一个账号没数据时不能崩，且所有值为 0
	c2, f2, _, e2, m2 := advWindowStat("不存在", from, now)
	if c2 != 0 || f2 != 0 || len(e2) != 0 || m2 != 10 {
		t.Errorf("空账号统计异常: calls=%d failed=%d errs=%v minutes=%d", c2, f2, e2, m2)
	}
}

// TestAdvTickDeltas 增量归桶：同一份当日累计只应该被记一次，不能重复计数。
func TestAdvTickDeltas(t *testing.T) {
	resetAdvBuckets(false)

	// 手动造两份"当日累计"的变化，直接测 advBucket 累加语义：
	// 第一次 tick 建立基线（advSeeded=false → 不写桶），第二次起写增量。
	advMu.Lock()
	advLastCum["providerA"] = [3]int64{100, 10, 50000}
	cur := [3]int64{112, 13, 56000}
	last := advLastCum["providerA"]
	d := [3]int64{cur[0] - last[0], cur[1] - last[1], cur[2] - last[2]}
	advBucketLocked("providerA", 1000).Add(d, map[string]int{"rpm_queue_timeout": 1})
	advLastCum["providerA"] = cur
	advMu.Unlock()

	// 再来一次同样的 cur（没有新调用）→ 增量应为 0，桶不该增长
	last2 := advLastCum["providerA"]
	d2 := [3]int64{cur[0] - last2[0], cur[1] - last2[1], cur[2] - last2[2]}
	if d2[0] != 0 || d2[1] != 0 || d2[2] != 0 {
		t.Errorf("重复采样产生了假增量: %v", d2)
	}

	advMu.Lock()
	b := advBuckets["providerA"][1000]
	advMu.Unlock()
	if b == nil || b.Calls != 12 || b.Failed != 3 || b.TimingMS != 6000 {
		t.Fatalf("桶内容不对: %+v（期望 calls=12 failed=3 timing=6000）", b)
	}
	if b.Errors["rpm_queue_timeout"] != 1 {
		t.Errorf("错误分类没记进桶: %v", b.Errors)
	}
}

// TestAdvAggregateByProvider 回归测试：统计表的 key 是 "provider:model"，
// 必须合并到账号维度。已经踩过的坑：按 "/" 拆分导致账号级调用数落进按模型的桶里，
// 账号窗口读出来全是 0（只剩错误计数），快照直接失真。
func TestAdvAggregateByProvider(t *testing.T) {
	stats := map[string]advisorModelStat{
		"providerA:model-a": {Provider: "providerA", Model: "model-a", Calls: 5, Failed: 1, TimingMS: 1000},
		"providerA:model-b": {Provider: "providerA", Model: "model-b", Calls: 8, Failed: 2, TimingMS: 4000},
		"providerD:model-c": {Provider: "providerD", Model: "model-c", Calls: 3, TimingMS: 300},
	}
	agg := advAggregateByProvider(stats)
	if got := agg["providerA"]; got != [3]int64{13, 3, 5000} {
		t.Errorf("providerA 聚合 = %v，期望 [13 3 5000]", got)
	}
	if got := agg["providerD"]; got != [3]int64{3, 0, 300} {
		t.Errorf("providerD 聚合 = %v，期望 [3 0 300]", got)
	}
	if len(agg) != 2 {
		t.Errorf("聚合后应只剩 2 个账号，实际 %d 个：%v（是不是又按模型拆开了？）", len(agg), agg)
	}
}

// TestAdvisorConcurrentDirectionGuard 这是实弹跑出来的教训：flash 级模型看到
// "排队超时上升"会建议**加**并发（还自相矛盾地写"增加并发"配 5→3）。
// 排队超时的瓶颈是上游每分钟名额，加并发只会让请求堆得更久 —— 必须拦下。
func TestAdvisorConcurrentDirectionGuard(t *testing.T) {
	cfg := &RuntimeConfig{
		AdvisorWindowMin: 10,
		Providers: []Provider{
			{Name: "providerB", Concurrent: 5, TimeoutSec: 60},
			{Name: "providerD", Concurrent: 3, TimeoutSec: 30},
		},
		Chain: []ChainStep{{Provider: "providerB", ModelName: "model-a"}},
	}

	// 灌入一个"排队严重"的窗口：10 分钟共 100 次调用 + 20 次排队超时（每百次 20 次）
	seed := func(calls, queuePerMinute int) {
		resetAdvBuckets(true)
		min := time.Now().Unix() / 60
		advMu.Lock()
		for i := 1; i <= 10; i++ {
			advBucketLocked("providerB", min-int64(i)).Add(
				[3]int64{int64(calls / 10), 2, 15000},
				map[string]int{"rpm_queue_timeout": queuePerMinute})
		}
		advMu.Unlock()
	}
	validate := func(target, to string) advisorChange {
		a := &advisorAdvice{Changes: []advisorChange{{Target: target, Field: "concurrent", From: "5", To: to}}}
		validateAdvisorAdvice(cfg, a)
		return a.Changes[0]
	}

	// 1) 排队严重时"加并发"→ 必须被拦下
	seed(100, 2)
	if c := validate("providerB", "6"); c.Valid {
		t.Errorf("每百次 20 次排队超时还建议加并发，居然通过了")
	} else if !strings.Contains(c.Error, "方向存疑") {
		t.Errorf("拦截原因不对: %s", c.Error)
	}
	// 2) 同一份数据"降并发"→ 应放行（这才是正确方向）
	if c := validate("providerB", "2"); !c.Valid {
		t.Errorf("降并发被误拦: %s", c.Error)
	}
	// 3) 排队不严重（每百次 5 次）时加并发 → 放行（别误伤）
	seed(100, 0)
	advMu.Lock()
	advBucketLocked("providerB", time.Now().Unix()/60-1).Add([3]int64{}, map[string]int{"rpm_queue_timeout": 5})
	advMu.Unlock()
	if c := validate("providerB", "6"); !c.Valid {
		t.Errorf("排队不严重时加并发被误拦: %s", c.Error)
	}
	// 4) 样本太少（窗口内几乎没有调用）→ 不作方向判断，放行
	seed(0, 0)
	if c := validate("providerB", "6"); !c.Valid {
		t.Errorf("样本不足时不应下方向结论: %s", c.Error)
	}
	// 5) 账号不存在时仍按原逻辑拒绝
	if c := validate("不存在", "6"); c.Valid {
		t.Errorf("不存在的账号应被拒绝")
	}
}

// TestAdvisorWindowMin 窗口取值的容错：未设/越界都要落到安全范围。
func TestAdvisorWindowMin(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, 10},  // 未设置 → 默认 10 分钟
		{-5, 10}, // 非法 → 默认
		{5, 5},   // 正常
		{120, 120},
		{999, 120}, // 超上限 → 截到 120
	}
	for _, c := range cases {
		cfg := &RuntimeConfig{AdvisorWindowMin: c.in}
		if got := advisorWindowMin(cfg); got != c.want {
			t.Errorf("advisorWindowMin(%d) = %d，期望 %d", c.in, got, c.want)
		}
	}
}

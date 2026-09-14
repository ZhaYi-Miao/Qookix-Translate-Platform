package main

import (
	"strings"
	"testing"
)

// TestLiveTracking 实时槽位埋点：在飞/等名额/等槽位分别计数，且"等名额"不占槽位。
func TestLiveTracking(t *testing.T) {
	liveMu.Lock()
	liveCalls = map[string]map[int64]*liveCall{}
	liveWaits = map[string]map[int64]*liveWait{}
	livePeak = map[string]int{}
	liveDone = 0
	liveMu.Unlock()

	cfg := &RuntimeConfig{
		Providers: []Provider{
			{Name: "providerA", Type: "openai", Concurrent: 5},
			{Name: "providerD", Type: "openai", Concurrent: 2},
		},
		Chain: []ChainStep{{Provider: "providerA", ModelName: "model-a", RPMPerMin: 20}},
	}
	applyRuntimeConfig(cfg)

	a1 := liveCallStart("providerA", "model-a", "body")
	a2 := liveCallStart("providerA", "model-a", "desc")
	a3 := liveCallStart("providerA", "model-a", "body")
	w1 := liveWaitStart("providerA", "model-a", "rpm")
	g1 := liveCallStart("providerD", "model-c", "desc")

	snap := liveSnapshot()
	rows := snap["accounts"].([]map[string]interface{})
	byName := map[string]map[string]interface{}{}
	for _, r := range rows {
		byName[r["account"].(string)] = r
	}
	// 账号顺序保持配置顺序，且没配的账号也要出现在列表里（面板要能显示"完全空闲"）
	if len(rows) != 2 || rows[0]["account"] != "providerA" || rows[1]["account"] != "providerD" {
		t.Fatalf("账号列表/顺序不对: %v", rows)
	}

	a1r := byName["providerA"]
	if a1r["inflight"].(int) != 3 || a1r["idle"].(int) != 2 {
		t.Errorf("providerA 在飞/空闲 = %v/%v，期望 3/2", a1r["inflight"], a1r["idle"])
	}
	if a1r["peak_inflight"].(int) != 3 {
		t.Errorf("providerA 峰值 = %v，期望 3", a1r["peak_inflight"])
	}
	// 关键：等名额的请求**不占槽位**，所以并发占用还是 3
	rw, ok := a1r["rpm_waiting"].(map[string]interface{})
	if !ok || rw["count"].(int) != 1 {
		t.Errorf("providerA 等名额计数不对: %v", a1r["rpm_waiting"])
	}
	if _, has := a1r["slot_waiting"]; has {
		t.Errorf("没人等槽位时不该出现 slot_waiting 字段")
	}
	// 在飞调用要带模型/用途，并按"最久的在前"排序
	calls := a1r["calls"].([]map[string]interface{})
	if len(calls) != 3 || calls[0]["kind"] == nil || calls[0]["sec"] == nil {
		t.Errorf("在飞调用明细不对: %v", calls)
	}

	g2 := byName["providerD"]
	if g2["inflight"].(int) != 1 || g2["idle"].(int) != 1 {
		t.Errorf("providerD 在飞/空闲 = %v/%v，期望 1/1", g2["inflight"], g2["idle"])
	}

	tot := snap["totals"].(map[string]interface{})
	if tot["inflight"].(int) != 4 || tot["waiting_rpm"].(int) != 1 || tot["waiting_slot"].(int) != 0 {
		t.Errorf("总量不对: %v", tot)
	}

	// 日志心跳行要能看出"在飞/等名额/等槽位"
	line := liveSummaryLine()
	if !strings.Contains(line, "providerA:在飞3/等名额1/等槽位0") || !strings.Contains(line, "providerD:在飞1") {
		t.Errorf("心跳行不对: %q", line)
	}

	// 全部结束后必须归零（不能漏删导致面板永远显示"占用中"）
	liveCallEnd("providerA", a1)
	liveCallEnd("providerA", a2)
	liveCallEnd("providerA", a3)
	liveCallEnd("providerD", g1)
	liveWaitEnd("providerA", w1)

	snap2 := liveSnapshot()
	tot2 := snap2["totals"].(map[string]interface{})
	if tot2["inflight"].(int) != 0 || tot2["waiting_rpm"].(int) != 0 {
		t.Errorf("结束后应归零，实际 %v", tot2)
	}
	if s := liveSummaryLine(); s != "" {
		t.Errorf("空闲时心跳行应为空，实际 %q", s)
	}
	if tot2["calls_done"].(int64) != 4 {
		t.Errorf("完成计数 = %v，期望 4", tot2["calls_done"])
	}
}

// TestLiveSnapshotShape 面板依赖的字段必须都在（前端拿不到就白屏）。
func TestLiveSnapshotShape(t *testing.T) {
	applyRuntimeConfig(&RuntimeConfig{
		Providers: []Provider{{Name: "P1", Type: "openai", Concurrent: 3}},
		Chain:     []ChainStep{{Provider: "P1", ModelName: "m1", RPMPerMin: 20}},
	})
	snap := liveSnapshot()
	for _, k := range []string{"now", "worker", "totals", "limits", "accounts"} {
		if _, ok := snap[k]; !ok {
			t.Errorf("快照缺字段 %s", k)
		}
	}
	lim := snap["limits"].(map[string]interface{})
	if lim["rpm_wait_timeout_sec"] == nil || lim["slot_wait_timeout_sec"] == nil {
		t.Errorf("limits 缺超时字段: %v", lim)
	}
	row := snap["accounts"].([]map[string]interface{})[0]
	for _, k := range []string{"account", "concurrent", "inflight", "idle", "calls", "cooling", "breakers"} {
		if _, ok := row[k]; !ok {
			t.Errorf("账号行缺字段 %s", k)
		}
	}
	if row["rpm"] == nil {
		t.Errorf("配了 RPM 的账号应带 rpm 信息（名额储备）")
	}
}

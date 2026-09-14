package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// demoSnapshot 是一份"真实形状"的快照（脱敏后），用来演示发给管家的内容。
// 数值取自一组真实运行数据（providerA 并发 5、RPM 20、窗口内排队超时 41 次等）。
const demoSnapshot = `{
  "schema": "trans-advisor/v1",
  "local_time": "2026-09-14 16:40 周一",
  "hour_of_day": 16,
  "window_minutes": 10,
  "accounts": [
    {"account":"providerA","chain_positions":[1,10],"concurrent":5,"rpm_limit":20,"cooldown_sec":60,
     "timeout_sec":60,"body_share":5,"today_calls":2908,"today_failed":793,"today_success":"72.7%",
     "window":{"minutes":10,"calls":76,"failed":21,"success":"72.4%","avg_ms":1460,"calls_per_min":7.6,
               "errors":{"rpm_queue_timeout":41,"cooldown_skip":12,"rate_limit":2},
               "queue_timeout":41,"queue_per_100c":53.9,"sample_coverage":"100%"},
     "prev_window":{"minutes":10,"calls":71,"failed":19,"success":"73.2%","avg_ms":1380,"queue_timeout":33},
     "trend":["调用 上升 71→76 次（↑7%）","排队超时 上升 33→41 次（↑24%）","平均延迟 上升 1380→1460 毫秒（↑6%）"],
     "ideal_concurrent":1,"rpm_utilization_pct":38,
     "slot_utilization":{"avg_inflight":0.2,"pct":4,"note":"<50% 说明槽位空转"}},
    {"account":"providerB","chain_positions":[2,9],"concurrent":5,"rpm_limit":20,"cooldown_sec":60,
     "body_share":5,"today_calls":595,"today_success":"88.7%",
     "window":{"minutes":10,"calls":33,"failed":4,"success":"87.9%","avg_ms":4700,"calls_per_min":3.3,
               "errors":{"rpm_queue_timeout":18,"rate_limit":1},
               "queue_timeout":18,"queue_per_100c":54.5,"sample_coverage":"100%"},
     "prev_window":{"minutes":10,"calls":30,"failed":3,"success":"90.0%","avg_ms":4600,"queue_timeout":9},
     "trend":["调用 基本持平 30→33 次","排队超时 上升 9→18 次（↑100%）"],
     "ideal_concurrent":3,"rpm_utilization_pct":17,
     "slot_utilization":{"avg_inflight":0.3,"pct":5}},
    {"account":"providerD","chain_positions":[5],"concurrent":3,"rpm_limit":0,
     "body_share":5,"today_calls":2302,"today_success":"67.6%",
     "window":{"minutes":10,"calls":58,"failed":19,"success":"67.2%","avg_ms":2600,"calls_per_min":5.8,
               "errors":{"rate_limit":6,"rate_limit_event":2},"rate_limit":8,"sample_coverage":"100%"},
     "prev_window":{"minutes":10,"calls":54,"failed":14,"success":"74.1%","avg_ms":2400},
     "trend":["调用 基本持平 54→58 次","平均延迟 上升 2400→2600 毫秒（↑8%）"],
     "concurrency_hint":"窗口内触发上游限速 6 次，并发偏大",
     "slot_utilization":{"avg_inflight":0.3,"pct":8}}
  ],
  "server": {"worker":{"type":"preheat","status":"done","ok":4986,"fail":0,"total":4987,"parallelism":26},
             "cache":{"desc_count":16576,"body_count":9935}},
  "hints":{"observation_window":"所有 window / prev_window 指标都是最近 10 分钟",
           "slot_utilization":">100% 说明请求在槽位外排队（并发不够）",
           "note":"免费账号排队随白天夜里变化大，请优先看趋势"}
}`

// demoAdvice 是"模拟 AI 看到上面快照后会返回什么"。
// 故意混进了 4 条坏建议：并发设 0、越界值、不存在的账号、链路少了一个账号、
// 以及一个不在白名单里的字段 —— 用来验证"读取识别"的护栏。
const demoAdvice = "好的，我看了这份快照，我的判断如下：\n" +
	"```json\n" +
	`{
  "summary": "免费档三个账号并发超配，多余请求在排队后漏给了链尾账号，建议下调并给链尾加保护",
  "health": "warn",
  "diagnosis": [
    "providerA 并发 5 但 RPM 只有 20（每 3 秒 1 个名额），5 个槽位里大部分时间在排队",
    "providerA/providerB/providerC 近 5 分钟各有 137/180/150 次排队超过 20 秒，这些请求全部落到了链尾账号",
    "providerD/providerE/providerH 今日成功率 65~70%，明显低于 providerF/providerG 的 88%，说明链上位置靠前、吃了太多溢出",
    "model-b 作为链尾兜底成功率 83~91%，保留合理"
  ],
  "changes": [
    {"target":"providerA","field":"concurrent","from":"5","to":"1","reason":"RPM 20 × 平均 1.46s ≈ 每 3 秒 1 次，1~2 个槽位足够喂满，给 5 个只会排队"},
    {"target":"providerB","field":"concurrent","from":"5","to":"2","reason":"平均延迟 4.7s，2~3 个槽位即可喂满 20/分"},
    {"target":"providerD","field":"concurrent","from":"3","to":"2","reason":"近 5 分钟触发限速，先降一档观察"},
    {"target":"providerA","field":"daily_limit","from":"0","to":"3000","reason":"给免费档留保护上限，避免整天打满被限"},
    {"target":"providerA","field":"concurrent","from":"5","to":"0","reason":"（故意非法的例子：并发不能为 0）"},
    {"target":"providerB","field":"rpm","from":"20","to":"999999","reason":"（故意越界的例子：上限 100000）"},
    {"target":"NOT_EXIST","field":"concurrent","from":"3","to":"1","reason":"（故意：账号不存在）"},
    {"target":"providerA","field":"chain_order","to":"providerA,providerB,providerH,providerD,providerE,providerF,providerG","reason":"（故意：链路少了 providerC）"},
    {"target":"providerA","field":"delete_account","to":"1","reason":"（故意：不在白名单的字段）"}
  ],
  "warnings": ["3.0-flash 延迟 5~10 秒，若把它提到链前会拖慢整体响应"],
  "confidence": 0.82
}
` + "```\n需要我进一步分析链路顺序的话告诉我。"

// TestAdvisorPromptMatchesSchema 提示词里承诺的字段必须和校验白名单一致，
// 否则模型照提示词输出、却被服务端判非法（改了一边忘了另一边就会这样）。
func TestAdvisorPromptMatchesSchema(t *testing.T) {
	fields := []string{"concurrent", "rpm", "rate_limit_cooldown_sec", "body_share", "daily_limit", "timeout_sec", "chain_order"}
	for _, f := range fields {
		if !strings.Contains(advisorSystemPrompt, f) {
			t.Errorf("提示词里没有说明字段 %s（校验白名单里有它）", f)
		}
	}
	keys := []string{"summary", "health", "diagnosis", "changes", "warnings", "confidence"}
	for _, k := range keys {
		if !strings.Contains(advisorSystemPrompt, k) {
			t.Errorf("提示词里没有给出输出字段 %s", k)
		}
	}
	// 提示词必须解释清楚快照里这些"要模型看懂"的字段，否则模型只会盯着裸数字猜
	for _, k := range []string{"window", "prev_window", "trend", "slot_utilization", "rpm_utilization_pct",
		"queue_per_100c", "sample_coverage", "window_minutes", "today_*", "禁止建议加并发"} {
		if !strings.Contains(advisorSystemPrompt, k) {
			t.Errorf("提示词里没有解释快照字段 %s 怎么读", k)
		}
	}
	// 输出结构体也要能承接提示词承诺的字段
	var a advisorAdvice
	raw := `{"summary":"s","health":"good","diagnosis":[],"changes":[],"warnings":[],"confidence":1}`
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatalf("输出结构体与提示词不一致: %v", err)
	}
}

// TestAdvisorDemoResponse 演示"读取识别"：把上面那份模拟返回解析出来 + 跑护栏。
func TestAdvisorDemoResponse(t *testing.T) {
	cfg := &RuntimeConfig{
		Providers: []Provider{
			{Name: "providerA", Concurrent: 5, TimeoutSec: 60},
			{Name: "providerB", Concurrent: 5, TimeoutSec: 60},
			{Name: "providerC", Concurrent: 2, TimeoutSec: 60},
			{Name: "providerH", Concurrent: 3, TimeoutSec: 30},
			{Name: "providerD", Concurrent: 3, TimeoutSec: 30},
			{Name: "providerE", Concurrent: 3, TimeoutSec: 30},
			{Name: "providerF", Concurrent: 2, TimeoutSec: 30},
			{Name: "providerG", Concurrent: 2, TimeoutSec: 30},
		},
		Chain: []ChainStep{
			{Provider: "providerA", ModelName: "model-a", RPMPerMin: 20, CooldownSec: 60, BodyShare: 5},
			{Provider: "providerB", ModelName: "model-a", RPMPerMin: 20, CooldownSec: 60, BodyShare: 5},
			{Provider: "providerC", ModelName: "model-a", RPMPerMin: 20, CooldownSec: 60, BodyShare: 5},
			{Provider: "providerH", ModelName: "model-c", BodyShare: 5},
			{Provider: "providerD", ModelName: "model-c", BodyShare: 5},
			{Provider: "providerE", ModelName: "model-c", BodyShare: 5},
			{Provider: "providerF", ModelName: "model-c", BodyShare: 5},
			{Provider: "providerG", ModelName: "model-c", BodyShare: 5},
			{Provider: "providerB", ModelName: "model-b", RPMPerMin: 20, CooldownSec: 60, BodyShare: 5},
			{Provider: "providerA", ModelName: "model-b", RPMPerMin: 20, CooldownSec: 60, BodyShare: 5},
			{Provider: "providerC", ModelName: "model-b", RPMPerMin: 20, CooldownSec: 60, BodyShare: 5},
		},
	}

	// 1) 识别：从"带解释文字 + ```json 包裹"的输出里抠出 JSON
	advice, err := parseAdvisorAdvice(demoAdvice)
	if err != nil {
		t.Fatalf("读取识别失败: %v", err)
	}
	if advice.Summary == "" || len(advice.Changes) != 9 {
		t.Fatalf("解析结果不对: summary=%q changes=%d", advice.Summary, len(advice.Changes))
	}
	t.Logf("管家结论: %s（health=%s 置信度=%.2f）", advice.Summary, advice.Health, advice.Confidence)
	for _, d := range advice.Diagnosis {
		t.Logf("  · %s", d)
	}

	// 2) 护栏：逐条判定合法/非法
	validateAdvisorAdvice(cfg, advice)
	want := []bool{true, true, true, true, false, false, false, false, false}
	for i, w := range want {
		c := advice.Changes[i]
		if c.Valid != w {
			t.Errorf("第 %d 条 %s.%s=%s valid=%v，期望 %v（%s）", i+1, c.Target, c.Field, c.To, c.Valid, w, c.Error)
		}
		if c.Valid {
			t.Logf("  可应用: %s.%s %s→%s （%s）", c.Target, c.Field, c.From, c.To, c.Reason)
		} else {
			t.Logf("  被拦下: %s.%s=%s → %s", c.Target, c.Field, c.To, c.Error)
		}
	}

	// 3) 应用：只落"通过校验"的 4 条，非法的一律不动
	changed := applyAdvisorChanges(cfg, advice)
	if len(changed) != 4 {
		t.Fatalf("应只应用 4 条，实际 %d: %v", len(changed), changed)
	}
	for _, c := range changed {
		t.Logf("  已应用: %s", c)
	}
	for _, p := range cfg.Providers {
		switch p.Name {
		case "providerA":
			if p.Concurrent != 1 {
				t.Errorf("providerA 并发应变成 1，实际 %d", p.Concurrent)
			}
		case "providerB":
			if p.Concurrent != 2 {
				t.Errorf("providerB 并发应变成 2，实际 %d", p.Concurrent)
			}
		case "providerD":
			if p.Concurrent != 2 {
				t.Errorf("providerD 并发应变成 2，实际 %d", p.Concurrent)
			}
		}
	}
	// 非法项不能生效：providerB 的 rpm 还是 20
	for _, st := range cfg.Chain {
		if st.Provider == "providerB" && st.RPMPerMin != 20 {
			t.Errorf("越界的 rpm 被错误应用: %d", st.RPMPerMin)
		}
	}
	// 链路没有被改短（那条 chain_order 少了 providerC，应被拦下）
	if len(cfg.Chain) != 11 {
		t.Errorf("链路步骤数被改动: %d", len(cfg.Chain))
	}
	if cfg.Chain[0].Provider != "providerA" || cfg.Chain[3].Provider != "providerH" {
		t.Errorf("链路顺序被非法改动: %s / %s", cfg.Chain[0].Provider, cfg.Chain[3].Provider)
	}
}

// TestApplyAdvisorChainOrder 合法调序要生效，且同一账号的多个步骤保持相对顺序。
func TestApplyAdvisorChainOrder(t *testing.T) {
	cfg := &RuntimeConfig{
		Providers: []Provider{{Name: "A"}, {Name: "B"}},
		Chain: []ChainStep{
			{Provider: "A", ModelName: "a1"}, {Provider: "B", ModelName: "b1"},
			{Provider: "A", ModelName: "a2"}, {Provider: "B", ModelName: "b2"},
		},
	}
	advice := &advisorAdvice{Changes: []advisorChange{
		{Target: "A", Field: "chain_order", To: "B,A"},
	}}
	changed := applyAdvisorChanges(cfg, advice)
	if len(changed) != 1 {
		t.Fatalf("应有 1 条变更，实际 %v", changed)
	}
	got := []string{}
	for _, st := range cfg.Chain {
		got = append(got, st.Provider+"/"+st.ModelName)
	}
	want := "B/b1,B/b2,A/a1,A/a2"
	if strings.Join(got, ",") != want {
		t.Errorf("调序结果 = %s，期望 %s", strings.Join(got, ","), want)
	}
}

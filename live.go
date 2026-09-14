package main

import (
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ============== 实时槽位视图 ==============
//
// 目的：把"每个账号的并发槽位此刻在干什么、有多少请求卡在拿名额的队列里"直接摆出来，
// 而不是让人（和模型）从统计数字里猜。
//
// 一个容易搞反的关键事实：**RPM 排队发生在拿并发槽位之前**（见 callChainIn 的顺序），
// 所以完全可能出现"槽位空着、请求却在排队"—— 免费额度账号上这就是常态，
// 也正是"排队超时高但加并发没用"的原因。这个卡片把两件事分开显示，就是为了让这一点可见。
//
// 埋点开销：三个 map 的加锁增删，纳秒级，且只在真正调用/等待时发生。

type liveCall struct {
	ID       int64
	Provider string
	Model    string
	Kind     string // desc / body / test / other
	Started  time.Time
}

type liveWait struct {
	ID       int64
	Provider string
	Model    string
	Kind     string // rpm（等每分钟名额）/ slot（等并发槽位）
	Started  time.Time
}

var (
	liveMu      sync.Mutex
	liveSeq     int64
	liveWaitSeq int64
	liveCalls   = map[string]map[int64]*liveCall{} // provider -> id -> 在飞调用
	liveWaits   = map[string]map[int64]*liveWait{} // provider -> id -> 正在等待的请求
	livePeak    = map[string]int{}                 // provider -> 本次进程内见过的并发峰值
	liveDone    int64                              // 累计完成的调用数（进程内）
	liveLastLog time.Time                          // [LIVE] 心跳日志上次打印时间（限频用）
)

func liveCallStart(provider, model, kind string) int64 {
	liveMu.Lock()
	defer liveMu.Unlock()
	liveSeq++
	id := liveSeq
	if liveCalls[provider] == nil {
		liveCalls[provider] = map[int64]*liveCall{}
	}
	liveCalls[provider][id] = &liveCall{ID: id, Provider: provider, Model: model, Kind: kind, Started: time.Now()}
	if n := len(liveCalls[provider]); n > livePeak[provider] {
		livePeak[provider] = n
	}
	return id
}

func liveCallEnd(provider string, id int64) {
	liveMu.Lock()
	defer liveMu.Unlock()
	if m := liveCalls[provider]; m != nil {
		delete(m, id)
		if len(m) == 0 {
			delete(liveCalls, provider)
		}
	}
	liveDone++
}

func liveWaitStart(provider, model, kind string) int64 {
	liveMu.Lock()
	defer liveMu.Unlock()
	liveWaitSeq++
	id := liveWaitSeq
	if liveWaits[provider] == nil {
		liveWaits[provider] = map[int64]*liveWait{}
	}
	liveWaits[provider][id] = &liveWait{ID: id, Provider: provider, Model: model, Kind: kind, Started: time.Now()}
	return id
}

func liveWaitEnd(provider string, id int64) {
	liveMu.Lock()
	defer liveMu.Unlock()
	if m := liveWaits[provider]; m != nil {
		delete(m, id)
		if len(m) == 0 {
			delete(liveWaits, provider)
		}
	}
}

// liveSummaryLine 汇总"有活动的账号"，供日志使用：没有面板凭据时也能确认埋点在跑。
func liveSummaryLine() string {
	liveMu.Lock()
	defer liveMu.Unlock()

	provs := map[string]bool{}
	for p, m := range liveCalls {
		if len(m) > 0 {
			provs[p] = true
		}
	}
	for p, m := range liveWaits {
		if len(m) > 0 {
			provs[p] = true
		}
	}
	if len(provs) == 0 {
		return ""
	}
	names := make([]string, 0, len(provs))
	for p := range provs {
		names = append(names, p)
	}
	sort.Strings(names)

	parts := make([]string, 0, len(names))
	for _, p := range names {
		rpm, slot := 0, 0
		for _, w := range liveWaits[p] {
			if w.Kind == "rpm" {
				rpm++
			} else {
				slot++
			}
		}
		parts = append(parts, fmt.Sprintf("%s:在飞%d/等名额%d/等槽位%d", p, len(liveCalls[p]), rpm, slot))
	}
	return strings.Join(parts, " ")
}

// liveLogSummary 有活动才打一行，空闲不打；而且最多每 2 分钟一行 ——
// 它只是"埋点还活着/现在的排队形状"的旁证，不该在长时间预热时把日志刷满。
func liveLogSummary() {
	liveMu.Lock()
	last := liveLastLog
	liveMu.Unlock()
	if time.Since(last) < 2*time.Minute {
		return
	}
	s := liveSummaryLine()
	if s == "" {
		return
	}
	liveMu.Lock()
	liveLastLog = time.Now()
	liveMu.Unlock()
	log.Printf("[LIVE] %s", s)
}

// ---- 快照（面板用）----

func handleLive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, liveSnapshot())
}

// liveWaitAgg 按 provider+kind 聚合等待中的请求。
type liveWaitAgg struct {
	n     int
	maxS  float64
	model string
}

func liveSnapshot() map[string]interface{} {
	cfg := getCfg()
	now := time.Now()

	// 先把状态拷出来，缩短持锁时间
	busy := map[string][]map[string]interface{}{}
	waits := map[string]*liveWaitAgg{} // key: provider|rpm 或 provider|slot
	peaks := map[string]int{}

	liveMu.Lock()
	for prov, m := range liveCalls {
		for _, c := range m {
			busy[prov] = append(busy[prov], map[string]interface{}{
				"model": c.Model,
				"kind":  c.Kind,
				"sec":   advRound1(time.Since(c.Started).Seconds()),
			})
		}
	}
	for prov, m := range liveWaits {
		for _, w := range m {
			key := prov + "|" + w.Kind
			a := waits[key]
			if a == nil {
				a = &liveWaitAgg{}
				waits[key] = a
			}
			sec := time.Since(w.Started).Seconds()
			a.n++
			if sec > a.maxS {
				a.maxS, a.model = sec, w.Model
			}
		}
	}
	for k, v := range livePeak {
		peaks[k] = v
	}
	done := liveDone
	liveMu.Unlock()

	totalInflight, totalWaitRPM, totalWaitSlot := 0, 0, 0
	rows := make([]map[string]interface{}, 0, len(cfg.Providers))

	for _, p := range cfg.Providers {
		calls := busy[p.Name]
		// 最久的排前面：一眼看出"谁卡住了"
		sort.Slice(calls, func(i, j int) bool {
			return calls[i]["sec"].(float64) > calls[j]["sec"].(float64)
		})

		rpmA, slotA := waits[p.Name+"|rpm"], waits[p.Name+"|slot"]
		rpmN, slotN := 0, 0
		if rpmA != nil {
			rpmN = rpmA.n
		}
		if slotA != nil {
			slotN = slotA.n
		}
		totalInflight += len(calls)
		totalWaitRPM += rpmN
		totalWaitSlot += slotN

		idle := p.Concurrent - len(calls)
		if idle < 0 {
			idle = 0
		}

		// 冷却 / 熔断：有剩余时间才列出来（直接看出"这个模型现在根本用不了"）
		cooling := []map[string]interface{}{}
		broken := []map[string]interface{}{}
		rpmLimit, rpmTokens := 0, 0.0
		for _, st := range cfg.Chain {
			if st.Provider != p.Name {
				continue
			}
			key := st.Provider + "/" + st.ModelName
			if left := rateLimitCooling(key); left > 0 {
				cooling = append(cooling, map[string]interface{}{"model": st.ModelName, "left_sec": int(left.Seconds() + 0.5)})
			}
			if left := getModelBreaker(st.Provider, st.ModelName).remaining(); left > 0 {
				broken = append(broken, map[string]interface{}{"model": st.ModelName, "left_sec": int(left.Seconds() + 0.5)})
			}
			// 同一账号的多个模型共用一个 RPM 桶，取第一个配了 RPM 的即可
			if rpmLimit == 0 {
				if r := stepRPM(st); r > 0 {
					rpmLimit = int(r)
					rpmTokens = advRound1(modelRPMLimiter(st).Tokens())
				}
			}
		}

		row := map[string]interface{}{
			"account":       p.Name,
			"provider_type": p.Type,
			"concurrent":    p.Concurrent,
			"inflight":      len(calls),
			"idle":          idle,
			"peak_inflight": peaks[p.Name],
			"calls":         calls,
			"cooling":       cooling,
			"breakers":      broken,
		}
		if rpmLimit > 0 {
			row["rpm"] = map[string]interface{}{"limit": rpmLimit, "tokens": rpmTokens}
		}
		if rpmN > 0 {
			row["rpm_waiting"] = map[string]interface{}{
				"count":     rpmN,
				"max_sec":   advRound1(rpmA.maxS),
				"max_model": rpmA.model,
				"note":      "在等每分钟名额，**没有**占用并发槽位（所以槽位空着也会排队）",
			}
		}
		if slotN > 0 {
			row["slot_waiting"] = map[string]interface{}{
				"count":     slotN,
				"max_sec":   advRound1(slotA.maxS),
				"max_model": slotA.model,
				"note":      "在等并发槽位：说明槽位真的不够用了",
			}
		}
		rows = append(rows, row)
	}

	workerState := map[string]interface{}{}
	workerMu.Lock()
	workerState["running"] = workerRunning
	workerMu.Unlock()
	pr := getWorkerProgress()
	workerState["phase"] = pr.Phase
	workerState["type"] = pr.Type
	workerState["status"] = pr.Status

	return map[string]interface{}{
		"now":    now.Format("15:04:05"),
		"worker": workerState,
		"totals": map[string]interface{}{
			"inflight":      totalInflight,
			"waiting_rpm":   totalWaitRPM,
			"waiting_slot":  totalWaitSlot,
			"calls_done":    done,
			"busy_accounts": activeAccountCount(busy),
		},
		"limits": map[string]interface{}{
			"slot_wait_timeout_sec": int(providerSemWaitTimeout.Seconds()),
			"rpm_wait_timeout_sec":  int(rpmWaitTimeout.Seconds()),
			"note":                  "等名额/等槽位超过这个秒数就放弃当前模型、换下一个（这就是「排队失败」的边界）",
		},
		"accounts": rows,
	}
}

func activeAccountCount(busy map[string][]map[string]interface{}) int {
	n := 0
	for _, v := range busy {
		if len(v) > 0 {
			n++
		}
	}
	return n
}

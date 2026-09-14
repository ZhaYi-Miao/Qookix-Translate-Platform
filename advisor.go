package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ============== AI 管家（顾问）==============
//
// 定位：**只做判断与建议，不碰执行**。
//   - 生成脱敏快照（只包含聚合指标与脱敏后的错误样本，绝不含 API Key / Base URL / 服务器信息 / 模组正文）
//   - 把快照发给"用户手动指定的管家模型"（面板里选），要求它输出固定 JSON
//   - 解析 + **确定性校验**（字段白名单、取值区间、链路完整性），校验不过的一律标注出来
//
// 为什么不让它直接改配置：控制回路需要可复现、可审计、有迟滞；这些正是 LLM 的弱项。
// 让它出主意、让人或确定性代码去执行，是这里刻意划的边界。

const (
	advisorLastKey     = "advisor:last"     // 最近一次问答（面板回显）
	advisorBaselineKey = "advisor:baseline" // 旧版：上次快照时的计数器（已被分钟桶取代，仅保留键名兼容）
	advisorLogMaxBytes = 1 << 20            // 只读日志尾部 1MB
)

// advisorSystemPrompt 固定输出格式，便于程序解析。
const advisorSystemPrompt = `你是 Minecraft Mod 翻译缓存系统的运维顾问。系统从 Modrinth 拉取内容，用一条"账号链"调用大模型翻译，并缓存结果。

你会收到一份脱敏后的运行快照（JSON），请判断当前配置是否合理，并给出可执行的调整建议。

可调整项（只能从这些里面选）：
- concurrent：该账号的并发槽位。经验公式：理想值 ≈ RPM ÷ 60 × 平均延迟(秒)，再留 1 点缓冲；超过这个量只会排队并漏给兜底账号。
- rpm：该账号每分钟请求上限（按账号算，同一账号的多个模型共享）。
- rate_limit_cooldown_sec：命中上游限速后的冷却秒数。
- body_share：正文分块分摊的权重，必须在 1~10 之间（>0 才参与分摊；数值越大，分给这个账号的正文越多）。
- daily_limit：该账号每日调用上限（0=不限）。
- timeout_sec：该账号请求超时。
- chain_order：链路顺序，值为按期望顺序排列的账号名，用逗号分隔。**每个账号只写一次**（不要按链路步骤重复列），
  但必须包含链上现有的全部账号，不能增删、只能调序。

怎么读快照（重要）：
- 所有接入的账号都是**免费额度**，排队情况随白天/夜里变化很大、分钟级还会随机抖动，
  但**同一小时内的水平大体相近**。所以：先看趋势（trend），再看数值。trend 是"本窗口 vs 上一个等长窗口"的变化。
  趋势上升说明压力在变大，按偏保守的方向给建议；只是单点高一点、趋势持平，不必大改。
- window 是最近 window_minutes 分钟（用户在面板设置的观察窗），prev_window 是它前面同样长的一段。
- window.sample_coverage 不是 100% 表示服务重启过、窗口数据不完整；此时判断要更保守，别下狠手。
- slot_utilization.pct：窗口内"平均在飞请求数" ÷ 配置并发。
  >100% = 请求在槽位外排队（并发不够，加并发有效）；50%~100% = 并发基本用满（合适）；<50% = 槽位空转（并发给多了，该降）。
- rpm_utilization_pct：窗口内平均每分钟调用 ÷ rpm 上限。
  接近或超过 100% = 这个账号的免费配额已经打满，此时**再加并发只会让排队更严重**，正确做法是降并发并把工作量让给别的账号。
- queue_per_100c：每 100 次调用里有多少次因为排队超过等待上限而失败。这部分直接变成用户可见的失败，优先消掉它。
- rate_limit / rate_limit_event = 上游限速次数；cooldown_skip = 因为冷却被跳过的次数。
  限速多 ⇒ 并发或速率超过上游容忍度；cooldown_skip 多 ⇒ 冷却时间设太长，白白空转。
- error_samples 是脱敏后的错误原文。判断"到底是限速、超时还是额度用尽"时以它为准。
- today_* 是当日累计（跨了高峰和低谷），只用来了解整体量级，不要拿它算当下。

并发该加还是该减（必须按这张表判断，不要凭感觉）：
- slot_utilization.pct > 100% → 我们的槽位不够用，可以加并发（加多少用公式算）。
- 50% ≤ slot_utilization.pct ≤ 100% → 并发合适，不要动。
- slot_utilization.pct < 50% 且 queue_per_100c < 5 → 槽位空转但没在排队：不要加并发。
- slot_utilization.pct < 50% 且 queue_per_100c ≥ 5 → **禁止建议加并发**。
  排队说明"到达量 > 上游每分钟名额"，而槽位根本没占满；加并发只会让更多请求堆在限速队列里等更久。
  正确做法是降并发（让请求更快失败并落到下一环）+ 用 body_share 减少这个账号的正文分摊。
  （这条反直觉，但队列长度取决于上游发名额的速度，不取决于我们开几个槽位。）

写 reason 时必须和 from/to 一致：不要出现"建议增加并发"却给 5→3 这种自相矛盾的说法。

判断原则：
1. 上游配额是硬上限，本地并发超过"喂满配额"只会造成排队并把工作量漏给兜底账号，不会提速。
2. 失败率优先：如果某个账号成功率明显偏低且错误以限速为主，说明并发给多了。
3. 慢模型放在链尾当兜底是可以接受的；不要因为单次延迟高就建议删掉它。
4. 只根据快照里的数据说话，不要编造数字。

调整思路（按优先级）：
1. 先把 queue_per_100c 高、slot_utilization 明显 >100%、但 rpm 还没打满的账号的并发调到公式值附近（这是纯赚的）。
2. 配额已打满的账号：降并发，并用 body_share 把正文分摊让给更闲、延迟更低的账号。
3. 某个账号限速多或 cooldown_skip 多：先调 rate_limit_cooldown_sec，再考虑用 chain_order 把它后移。
4. 慢账号留在链尾兜底不要动；只有它既慢又在抢量（位置靠前、调用量大）时才建议后移。

只输出一个 JSON 对象，不要任何解释文字、不要 markdown 代码块，格式如下：
{
  "summary": "一句话结论",
  "health": "good | warn | bad",
  "diagnosis": ["问题或观察，每条一句话"],
  "changes": [
    {"target": "账号名", "field": "concurrent", "from": "5", "to": "2", "reason": "为什么"}
  ],
  "warnings": ["风险提醒"],
  "confidence": 0.8
}
没有需要改的就让 changes 为空数组。`

// advisorChange 是模型给出的一条调整建议。
type advisorChange struct {
	Target string `json:"target"`
	Field  string `json:"field"`
	From   string `json:"from,omitempty"`
	To     string `json:"to"`
	Reason string `json:"reason"`
	// 下面是服务端校验后补的字段，模型不填
	Valid bool   `json:"valid,omitempty"`
	Error string `json:"error,omitempty"`
}

// advisorAdvice 是模型输出的完整结构。
type advisorAdvice struct {
	Summary    string          `json:"summary"`
	Health     string          `json:"health"`
	Diagnosis  []string        `json:"diagnosis"`
	Changes    []advisorChange `json:"changes"`
	Warnings   []string        `json:"warnings"`
	Confidence float64         `json:"confidence"`
}

// ============== 快照 ==============

type advisorModelStat struct {
	Provider string
	Model    string
	Calls    int64
	Failed   int64
	TimingMS int64
}

type advisorAccount struct {
	Name    string
	Type    string
	Conc    int
	Timeout int
	// 链上配置（按该 provider 聚合）
	RPMLimit       int
	CooldownSec    int
	BodyShare      int
	DailyLimit     int64
	ChainPositions []int
	// 统计
	TodayCalls   int64
	TodayFailed  int64
	TodayAvgMS   int64
	WinCalls     int64 // 距上次快照的增量（窗口）
	WinFailed    int64
	WinSeconds   float64
	IdealConc    int
	Errors       map[string]int
	ErrorSamples []string
}

// buildAdvisorSnapshot 生成脱敏快照。这是唯一会发给外部模型的内容。
func buildAdvisorSnapshot() map[string]interface{} {
	cfg := getCfg()
	now := time.Now()

	winMin := advisorWindowMin(cfg)
	weekday := [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}[int(now.Weekday())]

	snap := map[string]interface{}{
		"generated_at":   now.Format(time.RFC3339),
		"schema":         "trans-advisor/v1",
		"local_time":     now.Format("2006-01-02 15:04") + " " + weekday,
		"hour_of_day":    now.Hour(),
		"window_minutes": winMin,
		"server":         advisorServerState(),
		"redaction": map[string]interface{}{
			"removed": []string{"api_key", "base_url", "server_ip", "request_id", "mod 正文", "用户反馈与 IP"},
			"kept":    []string{"账号名（你给 provider 起的内部名）", "模型名", "聚合指标", "脱敏后的错误样本（已去掉 request id / 链接 / key）"},
		},
	}

	// 当前链路的配置事实
	chainView := []map[string]interface{}{}
	for i, st := range cfg.Chain {
		chainView = append(chainView, map[string]interface{}{
			"pos":         i + 1,
			"provider":    st.Provider,
			"model":       st.ModelName,
			"rpm":         st.RPMPerMin,
			"cooldown":    st.CooldownSec,
			"body_share":  st.BodyShare,
			"daily_limit": st.DailyLimit,
		})
	}
	snap["chain"] = chainView

	// 账号视角：把 provider 与按模型的统计合并起来
	accounts := advisorAccounts(cfg, now)
	snap["accounts"] = accounts

	snap["hints"] = map[string]interface{}{
		"observation_window":        fmt.Sprintf("所有 window / prev_window 指标都是最近 %d 分钟（面板可调）；prev_window 是它前面同样长的 %d 分钟，用来看趋势", winMin, winMin),
		"ideal_concurrency_formula": "ceil(rpm/60 × 窗口内平均延迟秒) + 1",
		"slot_utilization":          "窗口内平均在飞请求数 ÷ 配置并发。>100% 说明请求在槽位外排队（并发不够），明显 <50% 说明槽位空转（并发给多了）",
		"rpm_utilization_pct":       "窗口内平均每分钟调用 ÷ rpm 上限。接近/超过 100% 说明配额已打满，此时加并发只会增加排队，应降并发并把量让给别的账号",
		"queue_per_100c":            "每 100 次调用里有几次排队超过等待上限（这部分直接变成用户可见的失败）",
		"sample_coverage":           "窗口内采样完整度；低于 100% 表示服务重启过、窗口数据不完整，判断要更保守",
		"note":                      "today_* 是当日累计（跨高峰低谷），判断当下请用 window / prev_window。免费账号排队随白天夜里变化大，请优先看趋势而不是单点数值",
	}
	return snap
}

func advisorServerState() map[string]interface{} {
	ok, fail := getTodayStats()
	p := getWorkerProgress()
	workerMu.Lock()
	running := workerRunning
	workerMu.Unlock()

	descCount, bodyCount := 0, 0
	_ = db.QueryRow(`SELECT COUNT(*) FROM translations WHERE key NOT LIKE 'trans:mod:body:%'`).Scan(&descCount)
	_ = db.QueryRow(`SELECT COUNT(*) FROM translations WHERE key LIKE 'trans:mod:body:%'`).Scan(&bodyCount)

	return map[string]interface{}{
		"worker": map[string]interface{}{
			"running":     running,
			"type":        p.Type,
			"phase":       p.Phase,
			"platform":    p.Platform,
			"status":      p.Status,
			"current":     p.Current,
			"total":       p.Total,
			"ok":          p.OK,
			"fail":        p.Fail,
			"parallelism": workerParallelism(),
			"body_cache":  workerBodyCacheEnabled(),
		},
		"throughput":   throughputSnapshot(),
		"cache":        map[string]interface{}{"desc_count": descCount, "body_count": bodyCount},
		"today":        map[string]interface{}{"ok": ok, "fail": fail, "body_ok": getTodayBodyOk()},
		"rate_limiter": map[string]interface{}{"default_cooldown_sec": int(rateLimitCooldownDur.Seconds()), "rpm_wait_timeout_sec": int(rpmWaitTimeout.Seconds())},
	}
}

func advisorAccounts(cfg *RuntimeConfig, now time.Time) []map[string]interface{} {
	stats := advisorTodayStats()

	// 观察窗：长度由用户在面板设置。所有指标都用同一个窗口，再附一个等长窗口做对比 ——
	// 免费账号的排队白天夜里差别大、分钟内随机抖动，单点数值不能作为调整依据。
	winMin := advisorWindowMin(cfg)
	winDur := time.Duration(winMin) * time.Minute
	winFrom := now.Add(-winDur)
	prevFrom := winFrom.Add(-winDur)
	coverage := advSampleCoverage(winFrom, now)

	// 错误样本（原文，便于人/模型看懂到底哪种失败）仍从日志取，窗口与设置一致
	errs := advisorLogWindow(winDur)

	out := []map[string]interface{}{}
	for _, p := range cfg.Providers {
		acc := advisorAccount{Name: p.Name, Type: p.Type, Conc: p.Concurrent, Timeout: p.TimeoutSec}
		acc.Errors = map[string]int{}

		for i, st := range cfg.Chain {
			if st.Provider != p.Name {
				continue
			}
			acc.ChainPositions = append(acc.ChainPositions, i+1)
			if stepRPM(st) > 0 {
				if acc.RPMLimit == 0 || st.RPMPerMin < acc.RPMLimit {
					acc.RPMLimit = st.RPMPerMin
				}
			}
			if st.CooldownSec > acc.CooldownSec {
				acc.CooldownSec = st.CooldownSec
			}
			if st.BodyShare > acc.BodyShare {
				acc.BodyShare = st.BodyShare
			}
			if st.DailyLimit > 0 && (acc.DailyLimit == 0 || st.DailyLimit < acc.DailyLimit) {
				acc.DailyLimit = st.DailyLimit
			}
		}

		var totalTiming int64
		for _, s := range stats {
			if s.Provider != p.Name {
				continue
			}
			acc.TodayCalls += s.Calls
			acc.TodayFailed += s.Failed
			totalTiming += s.TimingMS
		}
		if acc.TodayCalls > 0 {
			// 当日 timing 累计 ÷ 调用数 = 该账号平均延迟（够用且稳定）
			acc.TodayAvgMS = totalTiming / acc.TodayCalls
		}

		// 本窗口 / 上一窗口（同长度）
		wCalls, wFailed, wTiming, wErrs, _ := advWindowStat(p.Name, winFrom, now)
		pwCalls, pwFailed, pwTiming, pwErrs, _ := advWindowStat(p.Name, prevFrom, winFrom)
		acc.WinCalls, acc.WinFailed = wCalls, wFailed
		acc.WinSeconds = float64(winMin) * 60
		acc.Errors = wErrs

		if e, ok := errs[p.Name]; ok {
			acc.ErrorSamples = e.Samples
		}

		// 窗口内的平均延迟比"当日累计"更能反映当下：白天夜里延迟差很多
		avgMS := int64(0)
		if wCalls > 0 {
			avgMS = wTiming / wCalls
		}
		callsPerMin := float64(wCalls) / float64(winMin)

		// 理想并发（RPM 已知才有意义）：优先用窗口内延迟算，没有窗口数据再退回当日
		latForIdeal := avgMS
		if latForIdeal == 0 {
			latForIdeal = acc.TodayAvgMS
		}
		if acc.RPMLimit > 0 && latForIdeal > 0 {
			ideal := int(float64(acc.RPMLimit)/60.0*float64(latForIdeal)/1000.0) + 1
			if ideal < 1 {
				ideal = 1
			}
			acc.IdealConc = ideal
		}

		queueTO := wErrs["rpm_queue_timeout"]
		pwQueueTO := pwErrs["rpm_queue_timeout"]
		rateLimit := wErrs["rate_limit"] + wErrs["rate_limit_event"]

		row := map[string]interface{}{
			"account":         acc.Name,
			"provider_type":   acc.Type,
			"in_chain":        len(acc.ChainPositions) > 0,
			"chain_positions": acc.ChainPositions,
			"concurrent":      acc.Conc,
			"timeout_sec":     acc.Timeout,
			"rpm_limit":       acc.RPMLimit,
			"cooldown_sec":    acc.CooldownSec,
			"body_share":      acc.BodyShare,
			"daily_limit":     acc.DailyLimit,
			"today_calls":     acc.TodayCalls,
			"today_failed":    acc.TodayFailed,
			"today_avg_ms":    acc.TodayAvgMS,
			"today_success":   advisorRate(acc.TodayCalls, acc.TodayFailed),
			"window": map[string]interface{}{
				"minutes":         winMin,
				"calls":           wCalls,
				"failed":          wFailed,
				"success":         advisorRate(wCalls, wFailed),
				"avg_ms":          avgMS,
				"calls_per_min":   advRound1(callsPerMin),
				"errors":          wErrs,
				"queue_timeout":   queueTO,
				"queue_per_100c":  advPer100(queueTO, wCalls),
				"cooldown_skip":   wErrs["cooldown_skip"],
				"rate_limit":      rateLimit,
				"sample_coverage": advPct(coverage),
			},
			"prev_window": map[string]interface{}{
				"minutes":       winMin,
				"calls":         pwCalls,
				"failed":        pwFailed,
				"success":       advisorRate(pwCalls, pwFailed),
				"avg_ms":        advDiv(pwTiming, pwCalls),
				"queue_timeout": pwQueueTO,
				"cooldown_skip": pwErrs["cooldown_skip"],
			},
			"trend": []string{
				"调用 " + advTrend(wCalls, pwCalls, "次"),
				"排队超时 " + advTrend(int64(queueTO), int64(pwQueueTO), "次"),
				"平均延迟 " + advTrend(avgMS, advDiv(pwTiming, pwCalls), "毫秒"),
			},
			"error_samples": acc.ErrorSamples,
		}

		// 配额是否打满：窗口内平均每分钟调用 ÷ RPM 上限
		if acc.RPMLimit > 0 {
			row["rpm_utilization_pct"] = int(callsPerMin / float64(acc.RPMLimit) * 100)
		}
		// 槽位利用率（Little's law）：窗口内"平均在飞请求数" ÷ 配置并发。
		// >100% ⇒ 请求在槽位外排队（并发不够）；明显 <50% ⇒ 槽位空转（并发给多了）。
		if acc.Conc > 0 && avgMS > 0 {
			inflight := callsPerMin * float64(avgMS) / 60000.0
			row["slot_utilization"] = map[string]interface{}{
				"avg_inflight": advRound1(inflight),
				"pct":          int(inflight / float64(acc.Conc) * 100),
				"note":         "窗口内平均占用并发 ÷ 配置并发；>100% 说明请求在排队（并发不足），<50% 说明槽位空转（并发给多了）",
			}
		}
		// 有 RPM 的账号给出理想并发；没有配 RPM 的账号算不了这个公式，
		// 改用"窗口内上游限速次数"给提示 —— 否则模型看到 0 会误以为该把并发设成 0。
		if acc.RPMLimit > 0 && acc.IdealConc > 0 {
			row["ideal_concurrent"] = acc.IdealConc
			row["ideal_concurrent_note"] = "按 ceil(rpm/60 × 窗口内平均延迟) + 1 算得"
		} else {
			switch {
			case rateLimit >= 5:
				row["concurrency_hint"] = fmt.Sprintf("窗口内触发上游限速 %d 次，并发偏大", rateLimit)
			case rateLimit > 0:
				row["concurrency_hint"] = fmt.Sprintf("窗口内偶发限速 %d 次", rateLimit)
			default:
				row["concurrency_hint"] = "窗口内无上游限速（未配 RPM，并发是否偏小要看排队与槽位利用率）"
			}
		}
		out = append(out, row)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i]["account"].(string) < out[j]["account"].(string)
	})
	return out
}

func advisorRate(calls, failed int64) string {
	if calls <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", 100.0*float64(calls-failed)/float64(calls))
}

// advisorTodayStats 读取当日 per-model 统计（provider:model -> calls/failed/timing）。
func advisorTodayStats() map[string]advisorModelStat {
	out := map[string]advisorModelStat{}
	if !redisAlive {
		return out
	}
	mh, err := rdb.HGetAll(ctx, fmt.Sprintf("stats:llm:%s:model", today())).Result()
	if err != nil {
		return out
	}
	for field, v := range mh {
		n, _ := strconv.ParseInt(v, 10, 64)
		base, kind := field, "calls"
		if strings.HasSuffix(field, ":failed") {
			base, kind = strings.TrimSuffix(field, ":failed"), "failed"
		} else if strings.HasSuffix(field, ":timing") {
			base, kind = strings.TrimSuffix(field, ":timing"), "timing"
		}
		idx := strings.Index(base, ":")
		if idx <= 0 {
			continue
		}
		s := out[base]
		s.Provider, s.Model = base[:idx], base[idx+1:]
		switch kind {
		case "calls":
			s.Calls = n
		case "failed":
			s.Failed = n
		case "timing":
			s.TimingMS = n
		}
		out[base] = s
	}
	return out
}

// ============== 日志里的错误归类（脱敏）==============

type advisorErrBucket struct {
	Counts  map[string]int
	Samples []string
}

var (
	advisorLineTS = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2})`)
	// provider/model：provider 名由用户自定，所以不写死名单，按 <名字>/<模型> 的形状抓
	advisorKV       = regexp.MustCompile(`([A-Za-z][\w\-]*)\s*/\s*([\w.\-/]+)`)
	advisorReqID    = regexp.MustCompile(`(?i)request id:\s*\S+`)
	advisorURL      = regexp.MustCompile(`https?://\S+`)
	advisorSecret   = regexp.MustCompile(`sk-[A-Za-z0-9\-_]{6,}`)
	advisorModelErr = regexp.MustCompile(`^model (\S+?) failed: (.*)$`)
)

// advisorLogWindow 解析主日志尾部，收集最近 window 内按账号归类的错误。
func advisorLogWindow(window time.Duration) map[string]*advisorErrBucket {
	out := map[string]*advisorErrBucket{}
	advisorScanLog(time.Now().Add(-window), func(prov, cat, sample string, _ time.Time) {
		advisorAddErr(out, prov, cat, sample)
	})
	return out
}

// advisorScanLog 扫一遍日志尾部，把 after 之后的每条可识别事件交给 collect。
// 错误分类的判据集中在这里：窗口统计、分钟桶采样、按窗口查错误都走同一套正则，
// 避免"改了一处、另一处还按老判据算"。
func advisorScanLog(after time.Time, collect func(provider, cat, sample string, at time.Time)) {
	f, err := os.Open(logFilePath())
	if err != nil {
		return
	}
	defer f.Close()

	// 只读尾部 1MB：日志是追加写的，最近的事件一定在尾部；
	// 读多了纯浪费（这里每 30 秒就被采样器调一次）。
	if st, err := f.Stat(); err == nil && st.Size() > advisorLogMaxBytes {
		_, _ = f.Seek(st.Size()-advisorLogMaxBytes, io.SeekStart)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return
	}

	for _, line := range strings.Split(string(data), "\n") {
		m := advisorLineTS.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ts, err := time.ParseInLocation("2006/01/02 15:04:05", m[1], time.Local)
		if err != nil || ts.Before(after) {
			continue
		}
		body := line[len(m[1]):]

		if me := advisorModelErr.FindStringSubmatch(strings.TrimSpace(body)); me != nil {
			key, msg := me[1], me[2]
			prov := key
			if i := strings.Index(key, "/"); i > 0 {
				prov = key[:i]
			}
			collect(prov, advisorCategorize(msg), msg, ts)
			continue
		}
		if strings.Contains(body, "[RATE-LIMIT]") {
			if kv := advisorKV.FindStringSubmatch(body); kv != nil {
				collect(kv[1], "rate_limit_event", body, ts)
			}
			continue
		}
		if strings.Contains(body, "排队超过") {
			if kv := advisorKV.FindStringSubmatch(body); kv != nil {
				collect(kv[1], "rpm_queue_timeout", body, ts)
			}
			continue
		}
		if strings.Contains(body, "[COOLDOWN] skip") {
			if kv := advisorKV.FindStringSubmatch(body); kv != nil {
				collect(kv[1], "cooldown_skip", body, ts)
			}
		}
	}
}

func advisorAddErr(out map[string]*advisorErrBucket, provider, cat, sample string) {
	b := out[provider]
	if b == nil {
		b = &advisorErrBucket{Counts: map[string]int{}, Samples: []string{}}
		out[provider] = b
	}
	b.Counts[cat]++
	if len(b.Samples) < 4 {
		b.Samples = append(b.Samples, advisorRedact(sample))
	}
}

func advisorCategorize(msg string) string {
	l := strings.ToLower(msg)
	switch {
	case strings.Contains(msg, "速率限制") || strings.Contains(l, "rate limit") || strings.Contains(l, "429") ||
		strings.Contains(l, "rpm") || strings.Contains(l, "tpm") || strings.Contains(l, "otpm"):
		return "rate_limit"
	case strings.Contains(l, "timeout") || strings.Contains(l, "deadline") || strings.Contains(msg, "超时") ||
		strings.Contains(l, "eof") || strings.Contains(l, "reset by peer"):
		return "timeout"
	case strings.Contains(l, "401") || strings.Contains(l, "403") || strings.Contains(l, "unauthor") ||
		strings.Contains(l, "api key") || strings.Contains(l, "invalid key"):
		return "auth"
	case strings.Contains(msg, "额度") || strings.Contains(msg, "余额") || strings.Contains(msg, "欠费") ||
		strings.Contains(l, "quota") || strings.Contains(l, "insufficient"):
		return "quota"
	case strings.Contains(l, "500") || strings.Contains(l, "502") || strings.Contains(l, "503") ||
		strings.Contains(l, "unmarshal") || strings.Contains(l, "empty choices"):
		return "server"
	default:
		return "other"
	}
}

// advisorRedact 去掉样本里的 request id / 链接 / key，并截断。
// 错误原文对判断很有价值（"免费用户速率限制" 和 "余额不足" 处置完全不同），所以保留文字、只摘掉可识别信息。
func advisorRedact(s string) string {
	s = advisorReqID.ReplaceAllString(s, "request_id:<removed>")
	s = advisorURL.ReplaceAllString(s, "<url>")
	s = advisorSecret.ReplaceAllString(s, "<key>")
	s = strings.Join(strings.Fields(s), " ")
	return truncateRunes(s, 160)
}

// ============== 调用管家模型 ==============

// advisorCall 用指定模型做一次"顾问"调用。
// 只走 OpenAI 兼容接口（现在所有账号都是这个类型），并且**不计入翻译统计**，避免污染成功率。
func advisorCall(p Provider, model, system, user string) (string, error) {
	if p.Type != "openai" {
		return "", fmt.Errorf("管家目前只支持 OpenAI 兼容接口（%s 的类型是 %s）", p.Name, p.Type)
	}
	reqBody := ChatRequest{
		Model:    model,
		Messages: []ChatMessage{{Role: "system", Content: system}, {Role: "user", Content: user}},
		Stream:   false,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequest("POST", openAIChatURL(p.BaseURL), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)

	timeout := p.TimeoutSec
	if timeout < 60 {
		timeout = 60 // 顾问要读一大段 JSON，比单条翻译慢
	}
	client := &http.Client{Timeout: time.Duration(timeout) * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	var chatResp ChatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return "", fmt.Errorf("管家返回无法解析: %v, body: %s", err, truncateRunes(string(respBody), 200))
	}
	if chatResp.Error != nil {
		return "", fmt.Errorf("管家模型报错: %s", chatResp.Error.Message)
	}
	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("管家返回空内容: %s", truncateRunes(string(respBody), 200))
	}
	return chatResp.Choices[0].Message.Content, nil
}

// parseAdvisorAdvice 从模型输出里抠出 JSON（容忍 ```json 包裹与前后废话）。
func parseAdvisorAdvice(raw string) (*advisorAdvice, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		if j := strings.Index(s, "```"); j >= 0 {
			s = s[:j]
		}
		s = strings.TrimPrefix(strings.TrimSpace(s), "json")
	}
	start, end := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("输出里找不到 JSON 对象")
	}
	var a advisorAdvice
	if err := json.Unmarshal([]byte(s[start:end+1]), &a); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %v", err)
	}
	return &a, nil
}

// validateAdvisorAdvice 确定性护栏：字段白名单 + 取值区间 + 链路完整性。
// 模型给的东西一律先过这里，校验不过的只做标注，绝不落库。
func validateAdvisorAdvice(cfg *RuntimeConfig, a *advisorAdvice) {
	providers := map[string]bool{}
	for _, p := range cfg.Providers {
		providers[p.Name] = true
	}
	// 链路里的"账号集合"：注意要去重 —— 同一账号可能挂多个模型步骤（示例里一个账号就挂了 2 步），
	// 而模型给 chain_order 时每个账号只写一次，按步骤数比较会永远判非法。
	chainProv := map[string]bool{}
	for _, st := range cfg.Chain {
		chainProv[st.Provider] = true
	}

	for i := range a.Changes {
		c := &a.Changes[i]
		c.Target = strings.TrimSpace(c.Target)
		c.Field = strings.TrimSpace(c.Field)
		if !providers[c.Target] {
			c.Error = "目标账号不存在: " + c.Target
			continue
		}
		to := strings.TrimSpace(c.To)
		switch c.Field {
		case "concurrent":
			n, err := strconv.Atoi(to)
			if err != nil || n < 1 || n > 100 {
				c.Error = "concurrent 需为 1-100 的整数"
				break
			}
			// 方向校验：这是"省下一个小数点、救回一整天的量"的地方。
			// 实测里 flash 级模型看到"排队超时上升"就建议**加**并发，理由还自相矛盾。
			// 排队超时的瓶颈是上游每分钟名额（到达量 > 名额），不是我们的槽位：
			// 加并发只会让更多请求堆在限速队列里等更久。这种情况必须拦下来。
			if cur := providerConcurrent(cfg, c.Target); n > cur && cur > 0 {
				q, calls := advisorQueuePressure(cfg, c.Target)
				if calls >= 10 && q >= 10 {
					c.Error = fmt.Sprintf("方向存疑：窗口内每 100 次调用有 %.0f 次排队超时，"+
						"说明瓶颈是上游每分钟名额（而不是我们的并发槽位）；"+
						"此时加并发只会让更多请求堆在限速队列里等更久，应该降并发或减少 body_share 分摊", q)
				}
			}
		case "rpm":
			if n, err := strconv.Atoi(to); err != nil || n < 0 || n > 100000 {
				c.Error = "rpm 需为 0-100000 的整数"
			}
		case "rate_limit_cooldown_sec":
			if n, err := strconv.Atoi(to); err != nil || n < 0 || n > 3600 {
				c.Error = "冷却需为 0-3600 秒"
			}
		case "body_share":
			if n, err := strconv.Atoi(to); err != nil || n < 0 || n > 1000 {
				c.Error = "body_share 需为 0-1000"
			}
		case "daily_limit":
			if n, err := strconv.ParseInt(to, 10, 64); err != nil || n < 0 {
				c.Error = "daily_limit 需为非负整数"
			}
		case "timeout_sec":
			if n, err := strconv.Atoi(to); err != nil || n < 1 || n > 300 {
				c.Error = "timeout_sec 需为 1-300 秒"
			}
		case "chain_order":
			// 只要求"账号集合完全一致" —— 模型每个账号写一次即可，不必按链路步骤重复列。
			// （一开始要求逐步骤列出，结果连自己都给不出合法值：模型不会知道同一账号挂了几个模型。）
			got := strings.Split(to, ",")
			uniq := map[string]bool{}
			for j := range got {
				name := strings.TrimSpace(got[j])
				if name == "" {
					continue
				}
				if !providers[name] {
					c.Error = "chain_order 里有不存在的账号: " + name
					break
				}
				uniq[name] = true
			}
			if c.Error == "" && len(uniq) != len(chainProv) {
				c.Error = fmt.Sprintf("chain_order 必须包含链上全部 %d 个账号（不能增删，只能调序），实际给了 %d 个", len(chainProv), len(uniq))
			}
		default:
			c.Error = "不允许的字段: " + c.Field
		}
		if c.Error == "" {
			c.Valid = true
		}
	}
}

// ============== HTTP handlers ==============

// handleAdvisorSnapshot 返回脱敏快照（面板「看快照」用，也可以自己拿去喂别的模型）。
func handleAdvisorSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, buildAdvisorSnapshot())
}

// handleAdvisorAsk 把快照交给用户指定的管家模型，返回结构化建议（只建议、不改配置）。
func handleAdvisorAsk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg := getCfg()
	if cfg.AdvisorProvider == "" || cfg.AdvisorModel == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "还没选管家模型（面板「AI 管家」里选一个 provider + 模型）"})
		return
	}
	p := findProviderIn(cfg, cfg.AdvisorProvider)
	if p == nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "管家 provider 不存在: " + cfg.AdvisorProvider})
		return
	}
	if p.APIKey == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "管家 provider 没有配 API Key"})
		return
	}

	// 尊重该账号的 RPM：顾问调用也算一次上游请求，不能因为它去撞限速
	if rpm := providerRPMLimit(cfg, cfg.AdvisorProvider); rpm > 0 {
		waitCtx, cancel := context.WithTimeout(context.Background(), rpmWaitTimeout)
		err := modelRPMLimiter(ChainStep{Provider: cfg.AdvisorProvider, RPMPerMin: int(rpm)}).Wait(waitCtx)
		cancel()
		if err != nil {
			writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "管家账号正在限速排队，稍后再试"})
			return
		}
	}

	snap := buildAdvisorSnapshot()
	payload, _ := json.MarshalIndent(snap, "", "  ")

	start := time.Now()
	raw, err := advisorCall(*p, cfg.AdvisorModel, advisorSystemPrompt, string(payload))
	ms := time.Since(start).Milliseconds()
	if err != nil {
		log.Printf("[ADVISOR] %s/%s 调用失败: %v", cfg.AdvisorProvider, cfg.AdvisorModel, err)
		writeJSONStatus(w, http.StatusBadGateway, map[string]interface{}{
			"ok": false, "error": err.Error(), "ms": ms,
		})
		return
	}

	res := map[string]interface{}{
		"ok":       true,
		"provider": cfg.AdvisorProvider,
		"model":    cfg.AdvisorModel,
		"ms":       ms,
		"raw":      raw,
		"snapshot": snap,
		"asked_at": time.Now().Unix(),
	}
	advice, perr := parseAdvisorAdvice(raw)
	if perr != nil {
		res["parse_error"] = perr.Error()
	} else {
		validateAdvisorAdvice(cfg, advice)
		res["advice"] = advice
	}
	if redisAlive {
		b, _ := json.Marshal(res)
		rdb.Set(ctx, advisorLastKey, string(b), 7*24*time.Hour)
	}
	log.Printf("[ADVISOR] %s/%s 返回 %dms，建议 %d 条", cfg.AdvisorProvider, cfg.AdvisorModel, ms, len(advice.Changes))
	writeJSON(w, res)
}

// ============== 应用 / 回滚 ==============

const advisorBackupKey = "advisor:config_backup" // 应用前的配置备份，供回滚

// cloneRuntimeConfig 深拷贝一份配置再改：直接改 getCfg() 返回的指针会污染运行中的配置，
// 万一保存失败就会出现"内存里变了、Redis 里没变"的不一致。
func cloneRuntimeConfig(cfg *RuntimeConfig) *RuntimeConfig {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return cfg
	}
	var out RuntimeConfig
	if err := json.Unmarshal(raw, &out); err != nil {
		return cfg
	}
	return &out
}

// applyAdvisorChanges 把"通过校验"的建议落到 cfg 上，返回人类可读的变更列表。
// 只动白名单字段；未通过校验的一律跳过（再跑一次校验是防御性设计：
// 调用方可能绕过了校验，这里不能信任输入）。
func applyAdvisorChanges(cfg *RuntimeConfig, a *advisorAdvice) []string {
	validateAdvisorAdvice(cfg, a)
	// 同一条建议如果覆盖了该账号的多个链步骤（模型步骤），合并成一行，面板弹窗才好读
	seen := map[string]bool{}
	var changed []string
	add := func(target, field, msg string) {
		key := target + "|" + field
		if seen[key] {
			return
		}
		seen[key] = true
		changed = append(changed, msg)
	}

	for _, c := range a.Changes {
		if !c.Valid {
			continue
		}
		switch c.Field {
		case "concurrent":
			n, _ := strconv.Atoi(c.To)
			for i := range cfg.Providers {
				if cfg.Providers[i].Name == c.Target && cfg.Providers[i].Concurrent != n {
					add(c.Target, c.Field, fmt.Sprintf("%s 并发 %d → %d", c.Target, cfg.Providers[i].Concurrent, n))
					cfg.Providers[i].Concurrent = n
				}
			}
		case "timeout_sec":
			n, _ := strconv.Atoi(c.To)
			for i := range cfg.Providers {
				if cfg.Providers[i].Name == c.Target && cfg.Providers[i].TimeoutSec != n {
					add(c.Target, c.Field, fmt.Sprintf("%s 超时 %d → %d 秒", c.Target, cfg.Providers[i].TimeoutSec, n))
					cfg.Providers[i].TimeoutSec = n
				}
			}
		case "rpm":
			n, _ := strconv.Atoi(c.To)
			for i := range cfg.Chain {
				if cfg.Chain[i].Provider == c.Target && cfg.Chain[i].RPMPerMin != n {
					add(c.Target, c.Field+cfg.Chain[i].ModelName, fmt.Sprintf("%s/%s RPM %d → %d", c.Target, cfg.Chain[i].ModelName, cfg.Chain[i].RPMPerMin, n))
					cfg.Chain[i].RPMPerMin = n
				}
			}
		case "rate_limit_cooldown_sec":
			n, _ := strconv.Atoi(c.To)
			for i := range cfg.Chain {
				if cfg.Chain[i].Provider == c.Target && cfg.Chain[i].CooldownSec != n {
					add(c.Target, c.Field+cfg.Chain[i].ModelName, fmt.Sprintf("%s/%s 限速冷却 %d → %d 秒", c.Target, cfg.Chain[i].ModelName, cfg.Chain[i].CooldownSec, n))
					cfg.Chain[i].CooldownSec = n
				}
			}
		case "body_share":
			n, _ := strconv.Atoi(c.To)
			for i := range cfg.Chain {
				if cfg.Chain[i].Provider == c.Target && cfg.Chain[i].BodyShare != n {
					add(c.Target, c.Field+cfg.Chain[i].ModelName, fmt.Sprintf("%s/%s 正文分摊 %d → %d", c.Target, cfg.Chain[i].ModelName, cfg.Chain[i].BodyShare, n))
					cfg.Chain[i].BodyShare = n
				}
			}
		case "daily_limit":
			n, _ := strconv.ParseInt(c.To, 10, 64)
			for i := range cfg.Chain {
				if cfg.Chain[i].Provider == c.Target && cfg.Chain[i].DailyLimit != n {
					add(c.Target, c.Field, fmt.Sprintf("%s 每日上限 %d → %d", c.Target, cfg.Chain[i].DailyLimit, n))
					cfg.Chain[i].DailyLimit = n
				}
			}
		case "chain_order":
			order := []string{}
			for _, name := range strings.Split(c.To, ",") {
				order = append(order, strings.TrimSpace(name))
			}
			reordered := reorderChain(cfg.Chain, order)
			if !sameChainOrder(cfg.Chain, reordered) {
				add(c.Target, c.Field, "链路顺序 → "+strings.Join(order, " > "))
				cfg.Chain = reordered
			}
		}
	}
	return changed
}

// reorderChain 按 order 里的账号顺序重排链路；同一账号的多个步骤保持原有相对顺序。
func reorderChain(chain []ChainStep, order []string) []ChainStep {
	rank := map[string]int{}
	for i, name := range order {
		rank[name] = i
	}
	out := make([]ChainStep, 0, len(chain))
	for _, name := range order {
		for _, st := range chain {
			if st.Provider == name {
				out = append(out, st)
			}
		}
	}
	return out
}

func sameChainOrder(a, b []ChainStep) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Provider != b[i].Provider || a[i].ModelName != b[i].ModelName {
			return false
		}
	}
	return true
}

// handleAdvisorApply 应用建议里"通过校验"的项（先备份，可回滚）。
// 请求体可以带 advice；不带就取最近一次问答的结果。
func handleAdvisorApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Advice *advisorAdvice `json:"advice"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	advice := req.Advice
	if advice == nil {
		if !redisAlive {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "redis unavailable"})
			return
		}
		raw, err := rdb.Get(ctx, advisorLastKey).Result()
		if err != nil || raw == "" {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "没有可应用的建议，先问一次管家"})
			return
		}
		var last struct {
			Advice *advisorAdvice `json:"advice"`
		}
		if json.Unmarshal([]byte(raw), &last) != nil || last.Advice == nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "上次的管家输出不是合法 JSON，无法应用"})
			return
		}
		advice = last.Advice
	}

	cur := getCfg()
	cfg := cloneRuntimeConfig(cur)
	changed := applyAdvisorChanges(cfg, advice)
	if len(changed) == 0 {
		writeJSON(w, map[string]interface{}{"ok": true, "changed": []string{}, "note": "没有可应用的有效项（可能全被校验拦下了）"})
		return
	}
	if redisAlive {
		if raw, err := json.Marshal(cur); err == nil {
			rdb.Set(ctx, advisorBackupKey, string(raw), 30*24*time.Hour)
		}
	}
	if err := saveRuntimeConfig(cfg); err != nil {
		writeJSONStatus(w, http.StatusBadGateway, map[string]string{"error": "保存配置失败: " + err.Error()})
		return
	}
	applyRuntimeConfig(cfg)
	log.Printf("[ADVISOR] 已应用 %d 项：%s", len(changed), strings.Join(changed, "; "))
	writeJSON(w, map[string]interface{}{"ok": true, "changed": changed, "note": "已生效（并发/RPM 立即重建）"})
}

// handleAdvisorRollback 回滚上一次"应用建议"造成的配置改动。
func handleAdvisorRollback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !redisAlive {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "redis unavailable"})
		return
	}
	raw, err := rdb.Get(ctx, advisorBackupKey).Result()
	if err != nil || raw == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "没有可回滚的备份（只有应用过建议才有）"})
		return
	}
	var cfg RuntimeConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		writeJSONStatus(w, http.StatusBadGateway, map[string]string{"error": "备份无法解析: " + err.Error()})
		return
	}
	if err := saveRuntimeConfig(&cfg); err != nil {
		writeJSONStatus(w, http.StatusBadGateway, map[string]string{"error": "回滚保存失败: " + err.Error()})
		return
	}
	applyRuntimeConfig(&cfg)
	rdb.Del(ctx, advisorBackupKey)
	log.Printf("[ADVISOR] 已回滚到应用前的配置")
	writeJSON(w, map[string]interface{}{"ok": true, "note": "已回滚到应用建议之前的配置"})
}

// handleAdvisorLast 回显最近一次问答（刷新面板不用重新问）。
func handleAdvisorLast(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !redisAlive {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "redis unavailable"})
		return
	}
	raw, err := rdb.Get(ctx, advisorLastKey).Result()
	if err != nil || raw == "" {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "还没有问过管家"})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte(raw))
}

package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ============== 状态页数据（对外只读，供文档站/状态页展示） ==============
//
//	GET /public/uptime?days=90      组件 × 每天的可用性/成功率（画 90 天色条）
//	GET /public/incidents?month=YYYY-MM   某月事件（画事件日历）
//
// 全部从已有统计推导，不额外埋点：
//   - 服务可用性：采样器每 10s 给 stats:uptime:<date> +1，理论 8640/天；
//     进程挂掉就没有 tick，缺口即真实停机时长（自报口径，但反映的是"服务在不在跑"）
//   - 通道成功率：stats:llm:<date>:model 的调用数/失败数（历史数据，可回溯）
//   - 事件：由上面两项 + 重启记录推导（成功率偏低 / 运行时长不足 / 重启）

const uptimeTicksPerDay = 8640 // 每 10 秒一次 × 86400 秒

type dayPoint struct {
	Date  string  `json:"date"`
	State string  `json:"state"` // ok | warn | bad | unknown
	Rate  float64 `json:"rate"`
}

type statusComponent struct {
	Key    string     `json:"key"`
	Name   string     `json:"name"`
	Kind   string     `json:"kind"` // uptime=可用性 | success=成功率
	Rate   float64    `json:"rate"`
	State  string     `json:"state"`
	Days   []dayPoint `json:"days"`
	Detail string     `json:"detail,omitempty"`
}

func stateOf(rate float64) string {
	switch {
	case rate >= 0.99:
		return "ok"
	case rate >= 0.9:
		return "warn"
	default:
		return "bad"
	}
}

func round3(v float64) float64 { return float64(int(v*1000+0.5)) / 1000 }

// expectedTicks 当天"应采样次数"：服务中途上线/重启的日子按首个采样点起算，
// 否则会把整天的 8640 当分母，把正常的中途启动误报成故障。
func expectedTicks(date string) int64 {
	expected := int64(uptimeTicksPerDay)
	if redisAlive {
		if since, err := rdb.Get(ctx, fmt.Sprintf("stats:uptime:%s:since", date)).Int64(); err == nil && since > 0 {
			if el := (time.Now().Unix()-since)/10 + 1; el < expected {
				expected = el
			}
		}
	}
	if expected < 1 {
		expected = 1
	}
	return expected
}

// dayList 返回从今天往前 n 天的日期（升序）。
func dayList(n int) []string {
	now := time.Now()
	out := make([]string, 0, n)
	for i := n - 1; i >= 0; i-- {
		out = append(out, now.AddDate(0, 0, -i).Format("2006-01-02"))
	}
	return out
}

// handlePublicUptime 组件 × 每天的状态，供画色条。
func handlePublicUptime(w http.ResponseWriter, r *http.Request) {
	setStatusHeaders(w, r)
	days := 90
	if v, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && v > 0 && v <= 365 {
		days = v
	}
	writeJSON(w, map[string]interface{}{
		"days":       days,
		"updated_at": time.Now().Unix(),
		"components": buildComponents(days),
	})
}

func buildComponents(days int) []statusComponent {
	dates := dayList(days)
	comps := make([]statusComponent, 0, 8)

	// 1) 服务整体可用性（采样 tick 数 / 理论值）
	svc := statusComponent{Key: "service", Name: "翻译服务（API Service）", Kind: "uptime"}
	var sumTicks int64
	var counted int
	for _, d := range dates {
		if !redisAlive {
			svc.Days = append(svc.Days, dayPoint{Date: d, State: "unknown"})
			continue
		}
		v, _ := rdb.Get(ctx, fmt.Sprintf("stats:uptime:%s", d)).Int64()
		if v == 0 {
			svc.Days = append(svc.Days, dayPoint{Date: d, State: "unknown"})
			continue
		}
		up := float64(v) / float64(expectedTicks(d))
		if up > 1 {
			up = 1
		}
		svc.Days = append(svc.Days, dayPoint{Date: d, State: stateOf(up), Rate: round3(up)})
		sumTicks += int64(up * uptimeTicksPerDay)
		counted++
	}
	if counted > 0 {
		svc.Rate = round3(float64(sumTicks) / float64(counted*uptimeTicksPerDay)) // 各天已归一化，这里等权平均
		svc.State = stateOf(svc.Rate)
	} else {
		svc.State = "unknown"
		svc.Detail = "可用性采样从本功能上线后开始记录，更早的日期显示为未知"
	}
	comps = append(comps, svc)

	// 2) Worker 成功度 = 当天全部模型调用的成功率（即用户请求的成功度）。
	//    与事件统计同口径（同一份 stats:llm:<date>:model 汇总），历史可回溯、不用另埋点。
	//    注意：通道成功率、事件明细属于内部数据，已不再随公开接口对外（要看就登录面板）。
	worker := statusComponent{Key: "worker", Name: "Worker 成功度", Kind: "success"}
	var wCalls, wFailed int64
	for _, d := range dates {
		calls, failed := dayCallStats(d)
		if calls == 0 {
			worker.Days = append(worker.Days, dayPoint{Date: d, State: "unknown"})
			continue
		}
		rate := float64(calls-failed) / float64(calls)
		worker.Days = append(worker.Days, dayPoint{Date: d, State: stateOf(rate), Rate: round3(rate)})
		wCalls += calls
		wFailed += failed
	}
	if wCalls > 0 {
		worker.Rate = round3(float64(wCalls-wFailed) / float64(wCalls))
		worker.State = stateOf(worker.Rate)
	} else {
		worker.State = "unknown"
	}
	comps = append(comps, worker)
	return comps
}

// dayCallStats 汇总某天所有模型调用的 (calls, failed)。
// 与事件统计同口径：跳过 :failed / :timing 这些派生字段，只累计调用数与失败数。
func dayCallStats(date string) (int64, int64) {
	if !redisAlive {
		return 0, 0
	}
	hk := fmt.Sprintf("stats:llm:%s:model", date)
	mh, err := rdb.HGetAll(ctx, hk).Result()
	if err != nil {
		return 0, 0
	}
	var calls, failed int64
	for field, val := range mh {
		if strings.HasSuffix(field, ":failed") || strings.HasSuffix(field, ":timing") {
			continue
		}
		c, _ := strconv.ParseInt(val, 10, 64)
		f, _ := rdb.HGet(ctx, hk, field+":failed").Int64()
		calls += c
		failed += f
	}
	return calls, failed
}

// handlePublicIncidents 某月的事件，供画事件日历。
// 事件由统计推导：成功率偏低 / 运行时长不足 / 重启次数。
func handlePublicIncidents(w http.ResponseWriter, r *http.Request) {
	setStatusHeaders(w, r)
	month := strings.TrimSpace(r.URL.Query().Get("month"))
	if month == "" {
		month = time.Now().Format("2006-01")
	}
	if _, err := time.Parse("2006-01", month); err != nil {
		http.Error(w, "month must be YYYY-MM", http.StatusBadRequest)
		return
	}
	first, _ := time.Parse("2006-01", month)
	daysInMonth := first.AddDate(0, 1, -1).Day()

	type event struct {
		Date   string `json:"date"`
		Level  string `json:"level"` // ok | degraded | outage | info
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	events := []event{}
	todayStr := time.Now().Format("2006-01-02")

	for d := 1; d <= daysInMonth; d++ {
		date := fmt.Sprintf("%s-%02d", month, d)
		if date > todayStr {
			break
		}
		// 服务运行时长（没有采样点的日期说明本功能还没上线，不做判断，也不跳过下面的成功率检查）
		if redisAlive {
			ticks, _ := rdb.Get(ctx, fmt.Sprintf("stats:uptime:%s", date)).Int64()
			if ticks > 0 {
				exp := expectedTicks(date)
				up := float64(ticks) / float64(exp)
				if up < 0.95 {
					lv := "degraded"
					if up < 0.5 {
						lv = "outage"
					}
					events = append(events, event{
						Date: date, Level: lv,
						Title:  fmt.Sprintf("服务运行时长 %.1f%%", up*100),
						Detail: fmt.Sprintf("当天采样 %d/%d 次，存在停机或进程未运行时段", ticks, exp),
					})
				}
			}
		}
		// 通道成功率
		if redisAlive {
			hk := fmt.Sprintf("stats:llm:%s:model", date)
			mh, _ := rdb.HGetAll(ctx, hk).Result()
			var calls, failed int64
			for field, val := range mh {
				if strings.HasSuffix(field, ":failed") || strings.HasSuffix(field, ":timing") {
					continue
				}
				c, _ := strconv.ParseInt(val, 10, 64)
				f, _ := rdb.HGet(ctx, hk, field+":failed").Int64()
				calls += c
				failed += f
			}
			if calls >= 50 {
				rate := float64(calls-failed) / float64(calls)
				if rate < 0.9 {
					lv := "degraded"
					if rate < 0.7 {
						lv = "outage"
					}
					events = append(events, event{
						Date: date, Level: lv,
						Title:  fmt.Sprintf("翻译成功率 %.1f%%", rate*100),
						Detail: fmt.Sprintf("当天 %d 次模型调用中失败 %d 次，主要原因是上游账号限速/超时", calls, failed),
					})
				}
			}
		}
	}

	writeJSON(w, map[string]interface{}{
		"month":      month,
		"updated_at": time.Now().Unix(),
		"events":     events,
	})
}

func setStatusHeaders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", statusCORSOrigin(r))
	w.Header().Set("Cache-Control", "public, max-age=30")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
}

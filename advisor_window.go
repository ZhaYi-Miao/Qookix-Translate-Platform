package main

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"
)

// ============== 管家窗口统计（分钟桶）==============
//
// 为什么需要这一层：
// 接入的多是免费档模型，排队是常态，而且白天夜里差别很大、
// 同一小时内又相对稳定、分钟级看就是随机抖动。要判断"现在该不该降并发"，
// 必须能把**任意长度的窗口**里的调用/失败/排队/限速/延迟拿出来，
// 并且和**上一个等长窗口**对比 —— 单点数值在这种抖动下会把人和模型都带偏。
//
// 原先的做法有两个尺度混在一起的问题：
//   - errors_5m 固定 5 分钟（写死，用户改不了）
//   - win_* 是"距上次快照"的增量（上次问管家可能是 30 分钟前）
// 模型看到"5 分钟排队 12 次"和"窗口 1800 秒 300 次调用"就没法算比例。
//
// 做法：每 advTickInterval 采一次"当日累计计数"的增量 + 日志里的错误事件，
// 按分钟归桶（粒度 1 分钟足够，用户设的窗口是 5/10/30 分钟级别），保留 2 小时。
// 快照时按窗口求和即可。桶同时落 Redis，服务重启能读回一部分；
// 快照里给出 coverage（窗口内实际有采样的分钟占比），数据不满就明说，别让模型猜。

const (
	advBucketMinutes   = 120 // 保留时长（分钟）：够 60 分钟窗口 + 它的前一窗口
	advTickInterval    = 30 * time.Second
	advBucketKey       = "advisor:buckets"
	advBucketSaveEvery = 60 * time.Second
	advMinWindowMin    = 1
	advMaxWindowMin    = 120
)

// advBucket 一分钟内某账号的量。Errors 是该分钟日志里出现的错误分类计数。
type advBucket struct {
	Calls    int64          `json:"c"`
	Failed   int64          `json:"f"`
	TimingMS int64          `json:"t"`
	Errors   map[string]int `json:"e,omitempty"`
}

type advErrEvent struct {
	At       time.Time
	Provider string
	Cat      string
}

var (
	advMu        sync.Mutex
	advBuckets   = map[string]map[int64]*advBucket{} // account -> unix 分钟 -> 桶
	advLastCum   = map[string][3]int64{}             // 上次采样时的当日累计 [calls, failed, timingMS]
	advSeeded    bool                                // 是否已建立基线（第一次采样只建基线，不写桶）
	advLogFrom   time.Time                           // 日志增量解析的起点
	advHeartbeat = map[int64]bool{}                  // 采样器实际跑过的分钟（算覆盖率用）
	advLastSave  time.Time
	advTickCount int // 诊断日志用
)

// startAdvisorBucketSampler 常驻采样（服务一起来就跑，与面板是否打开无关）。
func startAdvisorBucketSampler() {
	advRestore()

	go func() {
		time.Sleep(3 * time.Second) // 等 Redis / 配置就位
		advLogFrom = time.Now()
		advTick(time.Now())

		ticker := time.NewTicker(advTickInterval)
		defer ticker.Stop()
		for now := range ticker.C {
			advTick(now)
		}
	}()
	log.Printf("advisor sampler: bucket every %v, keep %d min", advTickInterval, advBucketMinutes)
}

// advTick 一次采样：累计计数做差 → 当前分钟桶；日志增量 → 错误分类桶。
func advTick(now time.Time) {
	minute := now.Unix() / 60

	// 1) 调用/失败/耗时：用"当日累计"的差值。跨零点归零时按"当前值即增量"处理。
	agg := advAggregateByProvider(advisorTodayStats())

	// 2) 错误事件：解析日志增量
	events := advisorLogEventsSince(advLogFrom)
	advLogFrom = now

	advMu.Lock()
	defer advMu.Unlock()

	for prov, cur := range agg {
		last, ok := advLastCum[prov]
		if !ok {
			advLastCum[prov] = cur
			continue
		}
		d := [3]int64{cur[0] - last[0], cur[1] - last[1], cur[2] - last[2]}
		if d[0] < 0 || d[1] < 0 || d[2] < 0 {
			// 跨零点 / 计数被重置：当作本轮全量
			d = cur
		}
		if d[0] > 0 || d[1] > 0 || d[2] > 0 {
			if advSeeded {
				advBucketLocked(prov, minute).Add(d, nil)
			}
		}
		advLastCum[prov] = cur
	}
	for _, ev := range events {
		if advSeeded {
			advBucketLocked(ev.Provider, ev.At.Unix()/60).Add([3]int64{}, map[string]int{ev.Cat: 1})
		}
	}
	advSeeded = true
	advHeartbeat[minute] = true

	// 3) 清理过期桶 / 心跳，定期落 Redis
	cutoff := minute - advBucketMinutes
	for acct, mins := range advBuckets {
		for m := range mins {
			if m < cutoff {
				delete(mins, m)
			}
		}
		if len(mins) == 0 {
			delete(advBuckets, acct)
		}
	}
	for m := range advHeartbeat {
		if m < cutoff {
			delete(advHeartbeat, m)
		}
	}
	// 诊断友好：刚启动的 10 个 tick 每次都记一行，之后每 20 个 tick 记一次。
	// 这条日志不写出来的话，"窗口里全是 0"是没法排查的（第一版就这么踩了）。
	advTickCount++
	if advTickCount <= 10 || advTickCount%20 == 0 {
		nb := 0
		for _, mins := range advBuckets {
			nb += len(mins)
		}
		log.Printf("[ADVISOR] sampler tick #%d: 账号=%d 日志事件=%d 桶=%d 已建基线=%v",
			advTickCount, len(agg), len(events), nb, advSeeded)
	}

	// 实时槽位心跳：有活动才打一行（无面板凭据时靠它确认埋点在工作）
	liveLogSummary()

	if now.Sub(advLastSave) >= advBucketSaveEvery {
		advLastSave = now
		advSaveLocked()
	}
}

func advBucketLocked(account string, minute int64) *advBucket {
	m := advBuckets[account]
	if m == nil {
		m = map[int64]*advBucket{}
		advBuckets[account] = m
	}
	b := m[minute]
	if b == nil {
		b = &advBucket{}
		m[minute] = b
	}
	return b
}

func (b *advBucket) Add(d [3]int64, errs map[string]int) {
	b.Calls += d[0]
	b.Failed += d[1]
	b.TimingMS += d[2]
	for cat, n := range errs {
		if b.Errors == nil {
			b.Errors = map[string]int{}
		}
		b.Errors[cat] += n
	}
}

// advWindowStat 求和 [from, to) 窗口内某账号的量。
// minutes 是该窗口内**采样器实际跑过**的分钟数，用于算覆盖率。
func advWindowStat(account string, from, to time.Time) (calls, failed, timing int64, errs map[string]int, minutes int) {
	errs = map[string]int{}
	fromMin, toMin := from.Unix()/60, to.Unix()/60

	advMu.Lock()
	defer advMu.Unlock()

	for m := fromMin; m < toMin; m++ {
		if advHeartbeat[m] {
			minutes++
		}
		b := advBuckets[account][m]
		if b == nil {
			continue
		}
		calls += b.Calls
		failed += b.Failed
		timing += b.TimingMS
		for cat, n := range b.Errors {
			errs[cat] += n
		}
	}
	return
}

// advSampleCoverage 返回窗口内采样器覆盖的分钟占比（0~1）。
func advSampleCoverage(from, to time.Time) float64 {
	fromMin, toMin := from.Unix()/60, to.Unix()/60
	total := toMin - fromMin
	if total <= 0 {
		return 0
	}
	advMu.Lock()
	defer advMu.Unlock()
	got := 0
	for m := fromMin; m < toMin; m++ {
		if advHeartbeat[m] {
			got++
		}
	}
	return float64(got) / float64(total)
}

// ---- Redis 持久化：重启后桶还能接上，把缺口压到最小 ----

func advSaveLocked() {
	if !redisAlive {
		return
	}
	raw, err := json.Marshal(advBuckets)
	if err != nil {
		return
	}
	rdb.Set(ctx, advBucketKey, string(raw), advBucketMinutes*2*time.Minute)
}

func advRestore() {
	if !redisAlive {
		return
	}
	raw, err := rdb.Get(ctx, advBucketKey).Result()
	if err != nil || raw == "" {
		return
	}
	var got map[string]map[int64]*advBucket
	if json.Unmarshal([]byte(raw), &got) != nil {
		return
	}
	advMu.Lock()
	for acct, mins := range got {
		if advBuckets[acct] == nil {
			advBuckets[acct] = map[int64]*advBucket{}
		}
		for m, b := range mins {
			if b != nil {
				advBuckets[acct][m] = b
			}
		}
	}
	advMu.Unlock()
	log.Printf("advisor sampler: restored %d accounts of buckets from redis", len(got))
}

// advisorLogEventsSince 解析日志尾部，返回 after 之后的错误事件（增量）。
// 与 advisorLogWindow 共用同一套正则，避免两处判据漂移。
func advisorLogEventsSince(after time.Time) []advErrEvent {
	var out []advErrEvent
	advisorScanLog(after, func(provider, cat, sample string, at time.Time) {
		out = append(out, advErrEvent{At: at, Provider: provider, Cat: cat})
	})
	return out
}

// advTrend 把"本窗口 vs 上一窗口"翻译成一句人话，模型和人都不容易看错。
func advTrend(cur, prev int64, unit string) string {
	if prev == 0 && cur == 0 {
		return "持平（都是 0）"
	}
	switch {
	case prev == 0:
		return fmt.Sprintf("从 0 升到 %d %s", cur, unit)
	case cur == 0:
		return fmt.Sprintf("从 %d 降到 0 %s", prev, unit)
	}
	ratio := float64(cur) / float64(prev)
	switch {
	case ratio >= 1.5:
		return fmt.Sprintf("上升 %s→%s %s（↑%.0f%%）", advNum(prev), advNum(cur), unit, (ratio-1)*100)
	case ratio <= 0.66:
		return fmt.Sprintf("下降 %s→%s %s（↓%.0f%%）", advNum(prev), advNum(cur), unit, (1-ratio)*100)
	default:
		return fmt.Sprintf("基本持平 %s→%s %s", advNum(prev), advNum(cur), unit)
	}
}

// advAggregateByProvider 把 per-model 的当日统计合并到账号维度。
//
// 注意：统计表的 key 是 "provider:model"（冒号分隔），必须用结构体里的 Provider 字段，
// 不能自己按 "/" 拆 —— 这条踩过坑：拆错了会让账号级调用数落进 "providerA:model-a"
// 这种桶里，账号窗口读出来全是 0，只剩错误计数，快照直接失真。
func advAggregateByProvider(stats map[string]advisorModelStat) map[string][3]int64 {
	agg := map[string][3]int64{}
	for _, s := range stats {
		prov := s.Provider
		if prov == "" {
			continue
		}
		cur := agg[prov]
		cur[0] += s.Calls
		cur[1] += s.Failed
		cur[2] += s.TimingMS
		agg[prov] = cur
	}
	return agg
}

// providerConcurrent 取某账号当前的并发配置（0 表示账号不存在）。
func providerConcurrent(cfg *RuntimeConfig, name string) int {
	for _, p := range cfg.Providers {
		if p.Name == name {
			return p.Concurrent
		}
	}
	return 0
}

// advisorQueuePressure 返回该账号在"用户设置的观察窗"内的
// 排队超时次数，以及窗口内调用数（用于判断样本是否足够）。
func advisorQueuePressure(cfg *RuntimeConfig, account string) (queuePer100 float64, calls int64) {
	winDur := time.Duration(advisorWindowMin(cfg)) * time.Minute
	now := time.Now()
	c, _, _, errs, _ := advWindowStat(account, now.Add(-winDur), now)
	if c <= 0 {
		return 0, 0
	}
	return advPer100(errs["rpm_queue_timeout"], c), c
}

// advisorWindowMin 观察窗长度（分钟）：面板可设，越界/未设走默认 10 分钟。
func advisorWindowMin(cfg *RuntimeConfig) int {
	n := cfg.AdvisorWindowMin
	if n < advMinWindowMin {
		n = 10
	}
	if n > advMaxWindowMin {
		n = advMaxWindowMin
	}
	return n
}

func advDiv(a, b int64) int64 {
	if b <= 0 {
		return 0
	}
	return a / b
}

func advRound1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}

// advPer100 每 100 次调用里出现多少次（比裸次数更能反映严重程度）。
func advPer100(n int, calls int64) float64 {
	if calls <= 0 {
		return 0
	}
	return advRound1(float64(n) * 100 / float64(calls))
}

func advPct(f float64) string {
	return fmt.Sprintf("%.0f%%", f*100)
}

func advNum(n int64) string {
	if n >= 10000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}

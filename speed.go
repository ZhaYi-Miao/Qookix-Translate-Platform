package main

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// ============== 服务端吞吐采样 ==============
//
// 以前"翻译速度"是前端拿 /worker/status 的相邻两次轮询做差算的，问题不少：
//   - 刷新页面就归零，要重新累计 20 秒样本；
//   - 单位是"个/分"，完全不反映真实工作量（一个 mod 的正文可能 1 个分块也可能 9 个）；
//   - 受页面轮询间隔、标签页节流影响，换台设备看到的数还不一样。
//
// 这里改成服务端常驻采样，口径固定：
//   字符 = 当日累计已翻译的**输入字符**（stats:llm:<date>:kind 里各用途 chars 之和，即真实翻译量）
//   个数 = 当日真正翻成功的 mod 数（worker:today:ok，不含跳过已缓存的）
// 面板拿到的就是权威值，跟哪个页面、哪个设备无关。

const (
	tpSampleInterval = 10 * time.Second
	tpWindow         = 5 * time.Minute
	tpMinWindow      = 20 * time.Second
	tpMaxPoints      = 200
)

type tpSample struct {
	at    time.Time
	chars int64
	mods  int64
}

var (
	tpMu     sync.Mutex
	tpPoints []tpSample
)

// translatedCharsToday 今日累计翻译的输入字符数（desc + body 合计）。
func translatedCharsToday() int64 {
	if !redisAlive {
		return 0
	}
	hk := fmt.Sprintf("stats:llm:%s:kind", today())
	m, err := rdb.HGetAll(ctx, hk).Result()
	if err != nil {
		return 0
	}
	var sum int64
	for field, v := range m {
		if !strings.HasSuffix(field, ":chars") {
			continue
		}
		var n int64
		fmt.Sscanf(v, "%d", &n)
		sum += n
	}
	return sum
}

func tpTake() tpSample {
	ok, _ := getTodayStats()
	return tpSample{at: time.Now(), chars: translatedCharsToday(), mods: ok}
}

// startThroughputSampler 常驻采样（跟页面是否打开无关，服务一起来就开跑）。
func startThroughputSampler() {
	go func() {
		// 先等 3 秒让 Redis/配置就位，否则第一个点可能是 0，会把速率算歪
		time.Sleep(3 * time.Second)
		first := tpTake()
		tpMu.Lock()
		tpPoints = []tpSample{first}
		tpMu.Unlock()

		ticker := time.NewTicker(tpSampleInterval)
		defer ticker.Stop()
		for range ticker.C {
			// 状态页的服务可用性采样：进程在跑才计数，缺口即停机。
			// 同时记录当天首个采样点时间，用来算"应采样次数"——
			// 否则服务中途上线/重启的日子会把整天 8640 当分母，误报成故障。
			if redisAlive {
				today := today()
				uk := fmt.Sprintf("stats:uptime:%s", today)
				pipe := rdb.Pipeline()
				pipe.Incr(ctx, uk)
				pipe.SetNX(ctx, uk+":since", time.Now().Unix(), 0)
				_, _ = pipe.Exec(ctx)
			}
			s := tpTake()
			tpMu.Lock()
			tpPoints = append(tpPoints, s)
			// 丢掉窗口外的旧点
			cutoff := time.Now().Add(-tpWindow)
			drop := 0
			for drop < len(tpPoints) && !tpPoints[drop].at.After(cutoff) {
				drop++
			}
			if drop > 0 {
				tpPoints = append([]tpSample(nil), tpPoints[drop:]...)
			}
			if len(tpPoints) > tpMaxPoints {
				tpPoints = tpPoints[len(tpPoints)-tpMaxPoints:]
			}
			tpMu.Unlock()
		}
	}()
	log.Printf("throughput sampler: sample every %v, window %v", tpSampleInterval, tpWindow)
}

// throughputSnapshot 返回最近窗口内的速率。
// ready=false 表示样本还不够（刚启动/Redis 不可用），面板可回退到前端估算。
func throughputSnapshot() map[string]interface{} {
	charsTotal := translatedCharsToday()
	modsTotal, _ := getTodayStats()
	out := map[string]interface{}{
		"ready":       false,
		"chars_total": charsTotal,
		"mods_total":  modsTotal,
	}

	tpMu.Lock()
	defer tpMu.Unlock()
	if len(tpPoints) < 2 {
		return out
	}
	first, last := tpPoints[0], tpPoints[len(tpPoints)-1]
	dt := last.at.Sub(first.at).Seconds()
	out["window_sec"] = int(dt)
	if dt < tpMinWindow.Seconds() {
		return out
	}

	chars := last.chars - first.chars
	mods := last.mods - first.mods
	if chars < 0 {
		chars = 0
	}
	if mods < 0 {
		mods = 0
	}
	out["ready"] = true
	out["sample_count"] = len(tpPoints)
	out["chars_per_min"] = int64(float64(chars) / dt * 60)
	out["mods_per_min"] = float64(mods) / dt * 60
	return out
}

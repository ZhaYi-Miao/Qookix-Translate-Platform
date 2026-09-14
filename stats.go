package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// statsTTL 统计数据 TTL：0 = 永久保留（不再按天清理，前端按「今天/昨天/近一周/近一月/全部」查询）。
// 历史键若还带旧 TTL，用 redis-cli 对 stats:* 执行 PERSIST 即可去掉。
const statsTTL = 0

// addExpire 按需给统计键设置 TTL；statsTTL<=0 时不设置（永久保留）。
func addExpire(pipe redis.Pipeliner, key string) {
	if statsTTL > 0 {
		pipe.Expire(ctx, key, statsTTL)
	}
}

func today() string {
	return time.Now().Format("2006-01-02")
}

func dateKey(prefix string) string {
	return fmt.Sprintf("stats:%s:%s", prefix, today())
}

func dateKeyWith(prefix, suffix string) string {
	return fmt.Sprintf("stats:%s:%s:%s", prefix, today(), suffix)
}

func statIncr(key string) {
	if !redisAlive {
		return
	}
	pipe := rdb.Pipeline()
	pipe.Incr(ctx, key)
	addExpire(pipe, key)
	_, _ = pipe.Exec(ctx)
}

func statIncrBy(key string, value int64) {
	if !redisAlive {
		return
	}
	pipe := rdb.Pipeline()
	pipe.IncrBy(ctx, key, value)
	addExpire(pipe, key)
	_, _ = pipe.Exec(ctx)
}

func statRequest(endpoint string)      { statIncr(dateKey("req:" + endpoint)) }
func statCacheHit()                    { statIncr(dateKeyWith("cache", "hit")) }
func statCacheMiss()                   { statIncr(dateKeyWith("cache", "miss")) }
func statOpenAICall()                  { statIncr(dateKeyWith("llm", "openai_calls")) }
func statOpenAIFailed()                { statIncr(dateKeyWith("llm", "openai_failed")) }
func statGeminiCall()                  { statIncr(dateKeyWith("llm", "gemini_calls")) }
func statGeminiFailed()                { statIncr(dateKeyWith("llm", "gemini_failed")) }
func statModrinthCall()                { statIncr(dateKeyWith("llm", "modrinth_calls")) }
func statModrinth404()                 { statIncr(dateKeyWith("llm", "modrinth_404")) }
func statModrinth429()                 { statIncr(dateKeyWith("llm", "modrinth_429")) }
func statRateLimitHit()                { statIncr(dateKeyWith("limit", "hits")) }
func statStaleFeedback(status string)  { statIncr(dateKeyWith("feedback_stale", status)) }
func statQualityFeedback(issue string) { statIncr(dateKeyWith("feedback_quality", issue)) }
func statOpenAITiming(ms int64)        { statIncrBy(dateKeyWith("timing", "openai_sum"), ms) }
func statGeminiTiming(ms int64)        { statIncrBy(dateKeyWith("timing", "gemini_sum"), ms) }

// statKindCall 按用途（body/desc）记录 LLM 调用次数、失败数和输入字符数，用于看两边成本。
func statKindCall(kind string, ok bool, chars int64) {
	if !redisAlive {
		return
	}
	hk := fmt.Sprintf("stats:llm:%s:kind", today())
	pipe := rdb.Pipeline()
	pipe.HIncrBy(ctx, hk, kind+":calls", 1)
	pipe.HIncrBy(ctx, hk, kind+":chars", chars)
	if !ok {
		pipe.HIncrBy(ctx, hk, kind+":failed", 1)
	}
	addExpire(pipe, hk)
	_, _ = pipe.Exec(ctx)
}

// statCacheKind 按用途（body/desc）记录缓存命中/未命中。
// key 布局与读取对齐：stats:cache:<date>:<kind>:<flag>
func statCacheKind(kind string, hit bool) {
	if !redisAlive {
		return
	}
	flag := "miss"
	if hit {
		flag = "hit"
	}
	statIncr(fmt.Sprintf("stats:cache:%s:%s:%s", today(), kind, flag))
}

// statReqWithBody 记录某翻译端点是否带了 include_body。
func statReqWithBody(endpoint string, withBody bool) {
	flag := "no_body"
	if withBody {
		flag = "with_body"
	}
	statIncr(dateKeyWith("reqbody:"+endpoint, flag))
}

// statLLMTrace 统一统计：按 provider 类型（OpenAI 兼容 / Gemini）累计总量（喂概览图表），
// 同时记一份 per-provider-per-model 计数（喂熔断表与每日预算）。
// 在 callOpenAI/callGemini 内各调用一次。
func statLLMTrace(typ, name, model string, ok bool, ms int64) {
	if typ == "gemini" {
		if ok {
			statGeminiCall()
			statGeminiTiming(ms)
		} else {
			statGeminiFailed()
		}
	} else {
		if ok {
			statOpenAICall()
			statOpenAITiming(ms)
		} else {
			statOpenAIFailed()
		}
	}
	recordModelStat(name, model, ok, ms)
}

// recordModelStat 对当日 per-model hash 记录 provider:model -> {total, failed}。
// key 布局：stats:llm:<date>:model  字段 provider:model -> total, provider:model:failed -> failed
func recordModelStat(provider, model string, ok bool, ms int64) {
	if !redisAlive {
		return
	}
	hk := fmt.Sprintf("stats:llm:%s:model", today())
	field := provider + ":" + model
	pipe := rdb.Pipeline()
	pipe.HIncrBy(ctx, hk, field, 1)
	if !ok {
		pipe.HIncrBy(ctx, hk, field+":failed", 1)
	}
	pipe.HIncrBy(ctx, hk, field+":timing", ms)
	addExpire(pipe, hk)
	_, _ = pipe.Exec(ctx)
}

// modelTodayUsed 返回某 provider/model 当日调用次数（预算判断用）。
func modelTodayUsed(provider, model string) int64 {
	if !redisAlive {
		return 0
	}
	hk := fmt.Sprintf("stats:llm:%s:model", today())
	v, _ := rdb.HGet(ctx, hk, provider+":"+model).Int64()
	return v
}

func statMod(modID string) {
	if !redisAlive {
		return
	}
	key := dateKey("mods")
	pipe := rdb.Pipeline()
	pipe.ZIncrBy(ctx, key, 1, modID)
	addExpire(pipe, key)
	_, _ = pipe.Exec(ctx)
}

func statIP(ip string) {
	if !redisAlive {
		return
	}
	key := dateKey("ip")
	pipe := rdb.Pipeline()
	pipe.HIncrBy(ctx, key, ip, 1)
	addExpire(pipe, key)
	_, _ = pipe.Exec(ctx)
}

type StatsResult struct {
	Date            string                   `json:"date"`
	Requests        map[string]int64         `json:"requests"`
	Cache           map[string]int64         `json:"cache"`
	CacheHitRate    string                   `json:"cache_hit_rate"`
	LLM             map[string]int64         `json:"llm"`
	OpenAIAvgMs     int64                    `json:"openai_avg_ms"`
	GeminiAvgMs     int64                    `json:"gemini_avg_ms"`
	RateLimitHits   int64                    `json:"rate_limit_hits"`
	StaleFeedback   map[string]int64         `json:"stale_feedback"`
	QualityFeedback map[string]int64         `json:"quality_feedback"`
	TopMods         []map[string]interface{} `json:"top_mods"`
	TopIPs          []map[string]interface{} `json:"top_ips"`
	BreakerOverview map[string]interface{}   `json:"breaker_overview,omitempty"`
	ModelStats      map[string]ModelStat     `json:"model_stats,omitempty"` // provider:model -> 当日用量/归属
	BreakerStatus   []map[string]interface{} `json:"breaker_status,omitempty"`

	// KindLLM 按用途拆分：desc_calls/desc_failed/desc_chars/body_calls/body_failed/body_chars
	KindLLM map[string]int64 `json:"kind_llm,omitempty"`

	// 本次统计覆盖的区间（今天/昨天/近 7 天/近 30 天/全部），供前端显示
	RangeLabel string `json:"range_label,omitempty"`
	RangeFrom  string `json:"range_from,omitempty"`
	RangeTo    string `json:"range_to,omitempty"`
	Days       int    `json:"days,omitempty"`

	// 区间汇总时用于加权平均的累加器（不导出，不参与 JSON）
	openaiMsSum int64
	openaiMsN   int64
	gemMsSum    int64
	gemMsN      int64
}

// ModelStat 是某 provider/model 的当日统计视图。
type ModelStat struct {
	Calls      int64 `json:"calls"`
	Failed     int64 `json:"failed"`
	DailyLimit int64 `json:"daily_limit"`
	AvgMs      int64 `json:"avg_ms"`
}

func getStats(date string) (*StatsResult, error) {
	if !redisAlive {
		return nil, fmt.Errorf("redis not available")
	}
	if date == "" {
		date = today()
	}

	res := &StatsResult{
		Date:            date,
		Requests:        make(map[string]int64),
		Cache:           make(map[string]int64),
		LLM:             make(map[string]int64),
		StaleFeedback:   make(map[string]int64),
		QualityFeedback: make(map[string]int64),
		TopMods:         []map[string]interface{}{},
		TopIPs:          []map[string]interface{}{},
	}

	getInt := func(key string) int64 {
		v, _ := rdb.Get(ctx, key).Int64()
		return v
	}

	for _, ep := range []string{"translate_mod", "translate_mods", "feedback_stale", "feedback_quality", "health"} {
		res.Requests[ep] = getInt(fmt.Sprintf("stats:req:%s:%s", ep, date))
	}
	var totalReq int64
	for _, v := range res.Requests {
		totalReq += v
	}
	res.Requests["total"] = totalReq

	// 请求是否带 body
	for _, ep := range []string{"translate_mod", "translate_mods"} {
		res.Requests[ep+"_with_body"] = getInt(fmt.Sprintf("stats:reqbody:%s:%s:with_body", ep, date))
		res.Requests[ep+"_no_body"] = getInt(fmt.Sprintf("stats:reqbody:%s:%s:no_body", ep, date))
	}

	// 缓存命中按 body/desc 拆分
	res.Cache["desc_hit"] = getInt(fmt.Sprintf("stats:cache:%s:desc:hit", date))
	res.Cache["desc_miss"] = getInt(fmt.Sprintf("stats:cache:%s:desc:miss", date))
	res.Cache["body_hit"] = getInt(fmt.Sprintf("stats:cache:%s:body:hit", date))
	res.Cache["body_miss"] = getInt(fmt.Sprintf("stats:cache:%s:body:miss", date))

	res.Cache["hit"] = getInt(fmt.Sprintf("stats:cache:%s:hit", date))
	res.Cache["miss"] = getInt(fmt.Sprintf("stats:cache:%s:miss", date))
	total := res.Cache["hit"] + res.Cache["miss"]
	if total > 0 {
		res.CacheHitRate = fmt.Sprintf("%.2f%%", float64(res.Cache["hit"])/float64(total)*100)
	} else {
		res.CacheHitRate = "N/A"
	}

	for _, k := range []string{"openai_calls", "openai_failed", "gemini_calls", "gemini_failed", "modrinth_calls", "modrinth_404", "modrinth_429"} {
		res.LLM[k] = getInt(fmt.Sprintf("stats:llm:%s:%s", date, k))
	}
	if res.LLM["openai_calls"] > 0 {
		sum := getInt(fmt.Sprintf("stats:timing:%s:openai_sum", date))
		res.OpenAIAvgMs = sum / res.LLM["openai_calls"]
	}
	if res.LLM["gemini_calls"] > 0 {
		sum := getInt(fmt.Sprintf("stats:timing:%s:gemini_sum", date))
		res.GeminiAvgMs = sum / res.LLM["gemini_calls"]
	}

	res.RateLimitHits = getInt(fmt.Sprintf("stats:limit:%s:hits", date))

	for _, s := range []string{"updated", "unchanged", "no_cache", "cooldown", "received", "error", "refresh_failed"} {
		res.StaleFeedback[s] = getInt(fmt.Sprintf("stats:feedback_stale:%s:%s", date, s))
	}
	for _, t := range []string{"wrong_translation", "unnatural", "missing", "other"} {
		res.QualityFeedback[t] = getInt(fmt.Sprintf("stats:feedback_quality:%s:%s", date, t))
	}

	mods, _ := rdb.ZRevRangeWithScores(ctx, fmt.Sprintf("stats:mods:%s", date), 0, 49).Result()
	for _, z := range mods {
		res.TopMods = append(res.TopMods, map[string]interface{}{
			"mod_id": z.Member,
			"count":  int64(z.Score),
		})
	}

	ipKey := fmt.Sprintf("stats:ip:%s", date)
	ipMap, _ := rdb.HGetAll(ctx, ipKey).Result()
	type ipCount struct {
		ip    string
		count int64
	}
	var ipList []ipCount
	for ip, v := range ipMap {
		var n int64
		fmt.Sscanf(v, "%d", &n)
		ipList = append(ipList, ipCount{ip, n})
	}
	sort.Slice(ipList, func(i, j int) bool {
		return ipList[i].count > ipList[j].count
	})
	limit := 50
	if len(ipList) < limit {
		limit = len(ipList)
	}
	for i := 0; i < limit; i++ {
		res.TopIPs = append(res.TopIPs, map[string]interface{}{
			"ip":    ipList[i].ip,
			"count": ipList[i].count,
		})
	}

	// 按用途（desc/body）拆分的 LLM 调用统计
	// Hash 字段以 ":" 分隔（desc:calls），对外统一转成 "_"（desc_calls），与前端字段约定一致。
	khk := fmt.Sprintf("stats:llm:%s:kind", date)
	res.KindLLM = map[string]int64{}
	if khm, err := rdb.HGetAll(ctx, khk).Result(); err == nil {
		for field, v := range khm {
			res.KindLLM[strings.ReplaceAll(field, ":", "_")], _ = strconv.ParseInt(v, 10, 64)
		}
	}

	// 填充 per-model 统计 + 每日上限（来自 Chain）
	hk := fmt.Sprintf("stats:llm:%s:model", date)
	mh, _ := rdb.HGetAll(ctx, hk).Result()
	limits := map[string]int64{}
	for _, st := range getCfg().Chain {
		limits[st.Provider+":"+st.ModelName] = st.DailyLimit
	}
	for field, v := range mh {
		if strings.HasSuffix(field, ":failed") || strings.HasSuffix(field, ":timing") {
			continue
		}
		calls, _ := strconv.ParseInt(v, 10, 64)
		failed, _ := rdb.HGet(ctx, hk, field+":failed").Int64()
		timing, _ := rdb.HGet(ctx, hk, field+":timing").Int64()
		var avg int64
		if calls > 0 {
			avg = timing / calls
		}
		if res.ModelStats == nil {
			res.ModelStats = map[string]ModelStat{}
		}
		res.ModelStats[field] = ModelStat{
			Calls:      calls,
			Failed:     failed,
			DailyLimit: limits[field],
			AvgMs:      avg,
		}
	}

	return res, nil
}

// ============== 区间统计（今天 / 昨天 / 近一周 / 近一月 / 全部） ==============

// emptyStats 建一个空的统计容器，字段与 getStats 的初始化保持一致。
func emptyStats() *StatsResult {
	return &StatsResult{
		Requests:        map[string]int64{},
		Cache:           map[string]int64{},
		LLM:             map[string]int64{},
		StaleFeedback:   map[string]int64{},
		QualityFeedback: map[string]int64{},
		KindLLM:         map[string]int64{},
		ModelStats:      map[string]ModelStat{},
		TopMods:         []map[string]interface{}{},
		TopIPs:          []map[string]interface{}{},
	}
}

// collectStatDates 从 SCAN 结果里抽取有数据的日期（yyyy-mm-dd，升序）。
// scan 做成可注入的，是为了能测"中间批次为空"这种情况 —— 实践中正是栽在这里：
//
// SCAN 是增量遍历，配合 MATCH 时**命中的键在键空间里是稀疏的**，
// 某一批返回空数组、而 cursor 还在继续往前，是完全正常的状态。
// 早先的代码把 len(keys)==0 当成"扫完了"直接 break，
// 结果只看到第一批命中的日期：区间被解析成"从今天开始"，
// 于是「近一周」「全部」的统计跟「今天」一模一样（用户看到的就是"历史数据好像丢了"）。
func collectStatDates(scan func(cursor uint64) ([]string, uint64, error)) []string {
	seen := map[string]bool{}
	var cursor uint64
	for i := 0; i < 500; i++ {
		keys, next, err := scan(cursor)
		if err != nil {
			break
		}
		for _, k := range keys {
			for _, part := range strings.Split(k, ":") {
				if len(part) == 10 && part[4] == '-' && part[7] == '-' {
					seen[part] = true
					break
				}
			}
		}
		// 只有 cursor 回到 0 才算扫完；空批次不能作为结束条件
		cursor = next
		if cursor == 0 {
			break
		}
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// statAvailableDates 扫描 stats:* 键收集有数据的日期，
// 避免为了「近一月/全部」去逐天空读 Redis。
func statAvailableDates() []string {
	return collectStatDates(func(cursor uint64) ([]string, uint64, error) {
		return rdb.Scan(ctx, cursor, "stats:*", 1000).Result()
	})
}

// resolveStatRange 把前端的区间标识解析成 [from, to] 与显示名。
func resolveStatRange(rng string) (from, to, label string) {
	now := time.Now()
	todayStr := now.Format("2006-01-02")
	switch rng {
	case "yesterday":
		d := now.AddDate(0, 0, -1).Format("2006-01-02")
		return d, d, "昨天"
	case "week":
		return now.AddDate(0, 0, -6).Format("2006-01-02"), todayStr, "近 7 天"
	case "month":
		return now.AddDate(0, 0, -29).Format("2006-01-02"), todayStr, "近 30 天"
	case "all":
		dates := statAvailableDates()
		if len(dates) == 0 {
			return todayStr, todayStr, "全部"
		}
		return dates[0], todayStr, "全部"
	default:
		return todayStr, todayStr, "今天"
	}
}

// mergeStats 把 src 累加进 dst；平均值按调用次数加权，命中率最后统一重算。
func mergeStats(dst, src *StatsResult) {
	for k, v := range src.Requests {
		dst.Requests[k] += v
	}
	for k, v := range src.Cache {
		dst.Cache[k] += v
	}
	for k, v := range src.LLM {
		dst.LLM[k] += v
	}
	for k, v := range src.StaleFeedback {
		dst.StaleFeedback[k] += v
	}
	for k, v := range src.QualityFeedback {
		dst.QualityFeedback[k] += v
	}
	if dst.KindLLM == nil {
		dst.KindLLM = map[string]int64{}
	}
	for k, v := range src.KindLLM {
		dst.KindLLM[k] += v
	}
	dst.RateLimitHits += src.RateLimitHits

	dst.openaiMsSum += src.OpenAIAvgMs * src.LLM["openai_calls"]
	dst.openaiMsN += src.LLM["openai_calls"]
	dst.gemMsSum += src.GeminiAvgMs * src.LLM["gemini_calls"]
	dst.gemMsN += src.LLM["gemini_calls"]

	if dst.ModelStats == nil {
		dst.ModelStats = map[string]ModelStat{}
	}
	for f, s := range src.ModelStats {
		cur := dst.ModelStats[f]
		total := cur.Calls + s.Calls
		var avg int64
		if total > 0 {
			avg = (cur.AvgMs*cur.Calls + s.AvgMs*s.Calls) / total
		}
		dst.ModelStats[f] = ModelStat{
			Calls:      total,
			Failed:     cur.Failed + s.Failed,
			DailyLimit: s.DailyLimit,
			AvgMs:      avg,
		}
	}

	// 热门 mod / IP：每日列表已截断到 50 条，长区间下为近似值
	mergeTopList(&dst.TopMods, src.TopMods, "mod_id")
	mergeTopList(&dst.TopIPs, src.TopIPs, "ip")
}

func mergeTopList(dst *[]map[string]interface{}, src []map[string]interface{}, idKey string) {
	acc := map[string]int64{}
	for _, m := range *dst {
		id, _ := m[idKey].(string)
		c, _ := m["count"].(int64)
		acc[id] += c
	}
	for _, m := range src {
		id, _ := m[idKey].(string)
		c, _ := m["count"].(int64)
		acc[id] += c
	}
	type kv struct {
		id string
		n  int64
	}
	list := make([]kv, 0, len(acc))
	for id, n := range acc {
		list = append(list, kv{id, n})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].n > list[j].n })
	if len(list) > 50 {
		list = list[:50]
	}
	out := make([]map[string]interface{}, 0, len(list))
	for _, e := range list {
		out = append(out, map[string]interface{}{idKey: e.id, "count": e.n})
	}
	*dst = out
}

// getStatsRange 把区间内每天的统计汇总成一份结果。
func getStatsRange(rng string) (*StatsResult, error) {
	if !redisAlive {
		return nil, fmt.Errorf("redis not available")
	}
	from, to, label := resolveStatRange(rng)
	res := emptyStats()
	res.Date = to
	res.RangeLabel = label
	res.RangeFrom = from
	res.RangeTo = to

	for _, d := range statAvailableDates() {
		if d < from || d > to {
			continue
		}
		day, err := getStats(d)
		if err != nil {
			continue
		}
		mergeStats(res, day)
		res.Days++
	}
	if res.Days == 0 {
		// 区间内暂无数据，至少读一次当天，保证结构完整
		if day, err := getStats(to); err == nil {
			mergeStats(res, day)
			res.Days = 1
		}
	}

	var totalReq int64
	for k, v := range res.Requests {
		if k != "total" {
			totalReq += v
		}
	}
	res.Requests["total"] = totalReq

	if res.openaiMsN > 0 {
		res.OpenAIAvgMs = res.openaiMsSum / res.openaiMsN
	}
	if res.gemMsN > 0 {
		res.GeminiAvgMs = res.gemMsSum / res.gemMsN
	}
	if hits := res.Cache["hit"] + res.Cache["miss"]; hits > 0 {
		res.CacheHitRate = fmt.Sprintf("%.2f%%", float64(res.Cache["hit"])/float64(hits)*100)
	} else {
		res.CacheHitRate = "N/A"
	}
	return res, nil
}

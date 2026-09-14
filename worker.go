package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	workerEnabledKey       = "worker:enabled"
	workerBodyCacheKey     = "worker:body_cache"
	workerPendingKey       = "worker:pending"
	workerProgressKey      = "worker:progress"
	workerTodayOkKey       = "worker:today:ok"
	workerTodayFailKey     = "worker:today:fail"
	workerTodayBodyOkKey   = "worker:today:body_ok"
	workerLastRunKey       = "worker:last_run"
	workerPreheatCursorKey = "worker:preheat:cursor"
	workerPreheatStepKey   = "worker:preheat:step"

	// 按来源的预热开关：worker:preheat:on:<platform>。
	// 默认开（不预热的话缓存永远是空的，客户端每次都要等实时翻译）。
	workerPreheatPlatformKeyPrefix = "worker:preheat:on:"

	// 自动续跑：一批跑完立即开下一批，把"点一次跑一批"变成"一路走到底"。
	// 默认关（预热会持续消耗账号额度，开关交给面板上的勾选）；没有任何可处理条目时自动停下。
	workerAutoContinueKey   = "worker:autocontinue"
	workerAutoContinueDelay = 15 * time.Second

	workerPreheatTopNDefault = 5000
	workerRefreshMax         = 500
	workerRetryCount         = 3
	workerRetryDelay         = 10 * time.Second
	workerRecentMax          = 50

	// 正文失败回访：正文是附加项，翻失败时该 mod 仍按成功计数、游标会跨过去，不回访就永久缺正文。
	// 失败时记进 Redis，本轮推进游标之前再补一轮；连续失败超过上限才放弃。
	workerBodyPendingKey        = "worker:body:pending"
	workerBodyPendingMaxRound   = 500
	workerBodyPendingMaxAttempt = 5
)

var workerLogger *log.Logger

func initWorkerLogger() {
	path := getEnv("WORKER_LOG_PATH", "./trans-worker.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("[WORKER] failed to open %s: %v, fallback to stdout", path, err)
		workerLogger = log.New(os.Stdout, "", 0)
		return
	}
	workerLogger = log.New(f, "", 0)
}

func wlog(format string, args ...interface{}) {
	ts := time.Now().Format("2006-01-02 15:04:05")
	msg := fmt.Sprintf(format, args...)
	if workerLogger != nil {
		workerLogger.Printf("[%s] %s", ts, msg)
	}
	log.Printf("[WORKER] %s", msg)
}

type WorkerProgress struct {
	Type      string   `json:"type"`
	Platform  string   `json:"platform"` // 当前批次处理的来源（当前只有 modrinth），面板据此显示
	Current   int      `json:"current"`
	Total     int      `json:"total"`
	ModID     string   `json:"mod_id"`
	OK        int      `json:"ok"`
	Fail      int      `json:"fail"`
	Status    string   `json:"status"`
	Phase     string   `json:"phase"`
	StartedAt int64    `json:"started_at"`
	Recent    []string `json:"recent"`
}

var (
	workerMu       sync.Mutex
	workerStopFlag bool
	workerRunning  bool
)

// ============== 基础状态 ==============

// workerFlag 读 Worker 的布尔开关：Redis 里有就用它，没有则回落到环境变量默认值（默认关闭）。
func workerFlag(key, envKey string) bool {
	return workerFlagDefault(key, envKey, "0")
}

// workerFlagDefault 与 workerFlag 相同，但可以指定"环境变量也没配"时的默认值。
func workerFlagDefault(key, envKey, def string) bool {
	if redisAlive {
		if v, err := rdb.Get(ctx, key).Result(); err == nil && v != "" {
			return v == "1"
		}
	}
	return getEnv(envKey, def) == "1"
}

func setWorkerFlag(key string, v bool) {
	if !redisAlive {
		return
	}
	val := "0"
	if v {
		val = "1"
	}
	rdb.Set(ctx, key, val, 0)
}

func workerEnabled() bool     { return workerFlag(workerEnabledKey, "WORKER_ENABLED") }
func setWorkerEnabled(v bool) { setWorkerFlag(workerEnabledKey, v) }

// workerBodyCacheEnabled 决定 Worker 是否连正文一起翻译缓存。
// 正文比描述贵得多（截断后最长 12000 字符、按 1500 分块 => 一个 mod 要 1~8 次模型调用），默认关闭。
func workerBodyCacheEnabled() bool { return workerFlag(workerBodyCacheKey, "WORKER_BODY_CACHE") }
func setWorkerBodyCache(v bool)    { setWorkerFlag(workerBodyCacheKey, v) }

// workerAutoContinue 决定一批预热跑完后是否自动开下一批（默认关）。
func workerAutoContinue() bool     { return workerFlag(workerAutoContinueKey, "WORKER_AUTOCONTINUE") }
func setWorkerAutoContinue(v bool) { setWorkerFlag(workerAutoContinueKey, v) }

// ============== 并发任务分发 ==============

// runJobsConcurrent 将 jobs 分发给 n 条 goroutine 并行处理（n = provider 并发之和）。
// 泛型是为了同时支持 []string（Modrinth slug、正文回访键）和 []modRef（带来源的任务）。
func runJobsConcurrent[T any](jobs []T, process func(job T)) {
	runJobsConcurrentN(jobs, workerParallelism(), process)
}

// runJobsConcurrentN 与 runJobsConcurrent 相同，但可以指定并发数。
func runJobsConcurrentN[T any](jobs []T, n int, process func(job T)) {
	if n < 1 {
		n = 1
	}
	jobsCh := make(chan T)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobsCh {
				if shouldStop() {
					return
				}
				process(j)
			}
		}()
	}
	for _, j := range jobs {
		if shouldStop() {
			break
		}
		jobsCh <- j
	}
	close(jobsCh)
	wg.Wait()
}

// ============== 增量预热 cursor/step（按来源各一套）==============

// preheatCursorKey/preheatStepKey 按来源分键：Modrinth 沿用历史键名（保留历史键名，老部署不用迁移游标），
// 其他来源加平台后缀。两个来源的排行榜长度完全不同（CF 只有前 1 万名），不能共用游标。
func preheatCursorKey(platform string) string {
	if platform == platformModrinth {
		return workerPreheatCursorKey
	}
	return workerPreheatCursorKey + ":" + platform
}

func preheatStepKey(platform string) string {
	if platform == platformModrinth {
		return workerPreheatStepKey
	}
	return workerPreheatStepKey + ":" + platform
}

// preheatPlatformEnabled 该来源是否参与预热。
func preheatPlatformEnabled(platform string) bool {
	def := "0"
	if platform == platformModrinth {
		def = "1" // 沿用历史行为：Modrinth 默认预热
	}
	return workerFlagDefault(workerPreheatPlatformKeyPrefix+platform, "WORKER_PREHEAT_"+strings.ToUpper(platform), def)
}

func setPreheatPlatformEnabled(platform string, v bool) {
	setWorkerFlag(workerPreheatPlatformKeyPrefix+platform, v)
}

// preheatPlatforms 本轮要预热的来源（按面板开关，顺序固定便于读日志）。
// 当前只有一个内容源；保留这层是因为预热流程、游标、面板控制都是按"来源"组织的。
func preheatPlatforms() []string {
	var out []string
	for _, p := range supportedPlatforms() {
		if preheatPlatformEnabled(p) {
			out = append(out, p)
		}
	}
	return out
}

func getPreheatCursorFor(platform string) int {
	if !redisAlive {
		return 0
	}
	v, _ := rdb.Get(ctx, preheatCursorKey(platform)).Int()
	return v
}

func setPreheatCursorFor(platform string, v int) {
	if !redisAlive {
		return
	}
	rdb.Set(ctx, preheatCursorKey(platform), v, 0)
}

func getPreheatStepFor(platform string) int {
	if !redisAlive {
		return workerPreheatTopNDefault
	}
	v, _ := rdb.Get(ctx, preheatStepKey(platform)).Int()
	if v <= 0 {
		return workerPreheatTopNDefault
	}
	return v
}

func setPreheatStepFor(platform string, v int) {
	if !redisAlive {
		return
	}
	if v <= 0 {
		v = workerPreheatTopNDefault
	}
	rdb.Set(ctx, preheatStepKey(platform), v, 0)
}

// ============== 进度 ==============

func getWorkerProgress() *WorkerProgress {
	p := &WorkerProgress{Status: "idle", Recent: []string{}}
	if !redisAlive {
		return p
	}
	raw, err := rdb.Get(ctx, workerProgressKey).Result()
	if err != nil || raw == "" {
		return p
	}
	if err := json.Unmarshal([]byte(raw), p); err != nil {
		return p
	}
	if p.Recent == nil {
		p.Recent = []string{}
	}
	return p
}

func saveWorkerProgress(p *WorkerProgress) {
	if !redisAlive {
		return
	}
	raw, _ := json.Marshal(p)
	rdb.Set(ctx, workerProgressKey, string(raw), 0)
}

func updateProgress(fn func(*WorkerProgress)) {
	workerMu.Lock()
	defer workerMu.Unlock()
	p := getWorkerProgress()
	fn(p)
	saveWorkerProgress(p)
}

// beginPhase 进入某个阶段时刷新进度快照。
// 关键：必须把 Status 置回 running —— 面板靠它判断"在跑"，而拉名单、回访正文这些分钟级阶段
// 如果沿用上一批的 stopped/done（以及旧的 total/current/started_at），看起来就像卡死了。
func beginPhase(taskType, phase string, total int) {
	updateProgress(func(p *WorkerProgress) {
		p.Type = taskType
		p.Phase = phase
		p.Status = "running"
		p.Current = 0
		p.Total = total
		p.ModID = ""
		p.StartedAt = time.Now().Unix()
	})
}

// beginPhaseFor 与 beginPhase 相同，但额外标明本批次的来源（多来源预热时面板要区分）。
func beginPhaseFor(platform, taskType, phase string, total int) {
	beginPhase(taskType, phase, total)
	updateProgress(func(p *WorkerProgress) { p.Platform = platform })
}

// fetchProgress 拉名单期间滚动更新进度，让面板里的进度条真的在动。
func fetchProgress(phase string, current, total int) {
	updateProgress(func(p *WorkerProgress) {
		p.Type = "preheat"
		p.Phase = phase
		p.Status = "running"
		p.Current = current
		p.Total = total
	})
}

// ============== 恢复 / 启动 / 停止 ==============

func resumeWorkerIfNeeded() {
	if !redisAlive {
		return
	}
	pending, _ := rdb.Get(ctx, workerPendingKey).Result()
	if pending == "" {
		return
	}
	if !workerEnabled() {
		wlog("[RESUME] pending task %s but worker disabled, skipping", pending)
		return
	}
	wlog("[RESUME] found pending task: %s, resuming in 5s", pending)
	time.Sleep(5 * time.Second)

	workerMu.Lock()
	already := workerRunning
	workerMu.Unlock()
	if already {
		return
	}

	// 必须走 startWorker：它会置 workerRunning=true。直接 go runWorker 会让 /worker/status
	// 一直报 running=false（面板因此隐藏进度条），还会让 /worker/start 的"已在运行"保护失效，
	// 理论上能并行起第二个预热任务。
	if err := startWorker(pending, nil); err != nil {
		wlog("[RESUME] resume task %s failed: %v", pending, err)
	}
}

func startWorker(taskType string, platforms []string) error {
	workerMu.Lock()
	if workerRunning {
		workerMu.Unlock()
		return fmt.Errorf("worker already running")
	}
	workerRunning = true
	workerStopFlag = false
	workerMu.Unlock()

	if redisAlive {
		rdb.Set(ctx, workerPendingKey, taskType, 0)
	}
	go runWorker(taskType, platforms)
	return nil
}

func stopWorker() {
	workerMu.Lock()
	workerStopFlag = true
	workerMu.Unlock()
	wlog("[STOP] stop requested, will stop after current mod")
}

func runWorker(taskType string, platforms []string) {
	defer func() {
		workerMu.Lock()
		workerRunning = false
		workerMu.Unlock()
	}()

	switch taskType {
	case "preheat":
		// platforms 为空表示"按面板开关跑所有启用的来源"
		if len(platforms) == 0 {
			platforms = preheatPlatforms()
		}
		runPreheat(platforms)
	case "refresh":
		runRefresh()
	default:
		wlog("[ERROR] unknown task type: %s", taskType)
	}
}

func shouldStop() bool {
	workerMu.Lock()
	defer workerMu.Unlock()
	return workerStopFlag
}

// ============== 预热（增量） ==============

// runPreheat 按来源逐个跑一批。
// 两个来源共用同一条模型链和同一份正文回访队列，串行跑更可预期：进度、日志、限速冷却都只对应一个来源，
// 出问题时不会互相掩盖（也避免两个来源同时把账号额度打满）。
func runPreheat(platforms []string) {
	if len(platforms) == 0 {
		// 两个来源都没勾时不要留 pending：否则重启后 RESUME 会拉起一个立刻空转的任务
		wlog("[PREHEAT] 没有启用的来源，跳过")
		updateProgress(func(p *WorkerProgress) {
			p.Type = "preheat"
			p.Status = "done"
			p.Phase = "没有启用的来源"
		})
		if redisAlive {
			rdb.Del(ctx, workerPendingKey)
		}
		return
	}

	// 开批先还上一批的欠账：正文失败的 mod 若只在整批跑完时才回访，遇到中途重启可能永远等不到。
	// 回访队列跨来源（键里带来源前缀），所以放在循环外只做一次。
	retryBodyPendingIfAny("开批回访")

	processed := 0 // 本批实际处理过的条目数（自动续跑据此判断有没有空转）

	for _, platform := range platforms {
		if shouldStop() {
			break
		}
		processed += runPreheatPlatform(platform)
		if !shouldStop() {
			retryBodyPendingIfAny(platform + " 批后回访")
		}
	}

	if shouldStop() {
		updateProgress(func(p *WorkerProgress) { p.Status = "stopped" })
		wlog("[PREHEAT] stopped by user")
	} else {
		updateProgress(func(p *WorkerProgress) { p.Status = "done"; p.ModID = "" })
	}
	if redisAlive {
		rdb.Del(ctx, workerPendingKey)
		rdb.Set(ctx, workerLastRunKey, time.Now().Unix(), 0)
	}

	// 自动续跑：把"一批"变成"一路走到底"（CF 按类型分批，一批只走 5000 个）。
	// 只在真的处理过东西时续跑 —— 全部类型都翻完后每批都是 0 条，续跑只会空转刷日志。
	// 注意 AfterFunc 里要再查一遍开关：延迟期间用户可能点了停止或取消了勾选。
	if !shouldStop() && processed > 0 && workerEnabled() && workerAutoContinue() {
		wlog("[AUTO] 本批处理 %d 条，%v 后自动续跑下一批（取消面板「自动续跑」或点停止即停）",
			processed, workerAutoContinueDelay)
		time.AfterFunc(workerAutoContinueDelay, func() {
			if shouldStop() || !workerEnabled() || !workerAutoContinue() {
				wlog("[AUTO] 续跑已取消（已停止或开关已关闭）")
				return
			}
			if err := startWorker("preheat", nil); err != nil {
				wlog("[AUTO] 续跑失败: %v", err)
				return
			}
			wlog("[AUTO] 已自动续跑下一批")
		})
	} else if !shouldStop() && processed == 0 {
		wlog("[AUTO] 本批没有任何可处理的条目，自动续跑不启动")
	}
}

// retryBodyPendingIfAny 有欠账就回访一轮正文；正文缓存关闭时直接跳过。
func retryBodyPendingIfAny(label string) {
	if !workerBodyCacheEnabled() {
		return
	}
	pending := takeBodyPending(workerBodyPendingMaxRound)
	if len(pending) == 0 {
		return
	}
	wlog("[BODY-RETRY] %s：%d 个正文失败的 mod", label, len(pending))
	retryBodyPending(pending)
	wlog("[BODY-RETRY] %s 结束，仍待补 %d 个", label, len(takeBodyPending(0)))
}

// runPreheatPlatform 跑一个来源的一批：从该来源的游标开始拉 step 个项目，返回本批实际处理的条目数
// （含成功/失败/已缓存跳过，但没有条目时返回 0 —— 自动续跑据此判断有没有空转）。
func runPreheatPlatform(platform string) int {
	cursor := getPreheatCursorFor(platform)
	step := getPreheatStepFor(platform)

	wlog("[PREHEAT] %s start, cursor=%d step=%d", platform, cursor, step)

	// 一开批就置为 running：拉 5000 个 mod 的名单要 1~2 分钟，这期间若不刷新状态，
	// 面板会一直显示上一批的"已停止"和旧进度条，看起来像 worker 卡死了。
	beginPhaseFor(platform, "preheat", "拉取待预热名单", step)

	// 从 cursor 开始（offset = cursor）
	refs, err := fetchTopModsFrom(platform, step, cursor)
	if err != nil {
		wlog("[PREHEAT] %s fetch top mods failed: %v", platform, err)
		updateProgress(func(p *WorkerProgress) { p.Status = "done"; p.Type = "preheat" })
		return 0
	}

	if len(refs) == 0 {
		wlog("[PREHEAT] %s no more mods to fetch (cursor=%d)", platform, cursor)
		updateProgress(func(p *WorkerProgress) {
			p.Type = "preheat"
			p.Status = "done"
			p.Current = 0
			p.Total = 0
			p.ModID = ""
		})
		return 0
	}

	wlog("[PREHEAT] %s got %d mods from offset %d", platform, len(refs), cursor)

	beginPhaseFor(platform, "preheat", "翻译中", len(refs))
	updateProgress(func(p *WorkerProgress) {
		p.OK = 0
		p.Fail = 0
		p.Recent = []string{}
	})

	withBody := workerBodyCacheEnabled()
	var done atomic.Int64
	runJobsConcurrent(refs, func(ref modRef) {
		// 已处理计数（含成功/失败/已缓存），用于 cursor 推进
		updateProgress(func(p *WorkerProgress) {
			p.ModID = ref.ID
		})

		// 描述已缓存时：没开正文缓存就直接跳过；开了正文缓存则正文也缓存好了才算处理完，
		// 否则「已经预热过描述的 mod」永远补不上正文。
		if cacheHasText(modCacheKey(ref.Platform, ref.ID, "zh")) &&
			(!withBody || cacheHasText(modBodyCacheKey(ref.Platform, ref.ID, "zh"))) {
			done.Add(1)
			count := int(done.Load())
			updateProgress(func(p *WorkerProgress) { p.Current = count })
			return
		}

		err := workerTranslateMod(ref, "zh")
		if err != nil {
			wlog("[PREHEAT] %s %s ✗ %v", ref.Platform, ref.ID, err)
			incTodayFail()
			updateProgress(func(p *WorkerProgress) {
				p.Fail++
				p.Recent = appendRecent(p.Recent, fmt.Sprintf("✗ %s (%s)", ref.ID, shortErr(err)))
			})
		} else {
			wlog("[PREHEAT] %s %s ✓", ref.Platform, ref.ID)
			incTodayOk()
			updateProgress(func(p *WorkerProgress) {
				p.OK++
				p.Recent = appendRecent(p.Recent, fmt.Sprintf("✓ %s", ref.ID))
			})
		}
		done.Add(1)
		count := int(done.Load())
		updateProgress(func(p *WorkerProgress) { p.Current = count })
	})

	// 游标推进放在最后（正文回访由 runPreheat 在批后统一做，避免两处重复回访）
	doneCount := int(done.Load())
	if shouldStop() {
		// 中途停止不推进 cursor：并发下完成数不保证是连续前缀，推进会把没跑到的 mod 跳过去。
		// 下次从本批开头重跑，已缓存的会被自动跳过，不会重复消耗额度。
		wlog("[PREHEAT] %s stopped: ok=%d fail=%d, cursor 保持 %d（下次从本批开头续跑）",
			platform, getProgressOK(), getProgressFail(), cursor)
		return doneCount
	}
	nextCursor := cursor + doneCount
	wlog("[PREHEAT] %s finished: ok=%d fail=%d, cursor %d -> %d",
		platform, getProgressOK(), getProgressFail(), cursor, nextCursor)
	setPreheatCursorFor(platform, nextCursor)
	return doneCount
}

// ============== 更新检测 ==============

func runRefresh() {
	wlog("[REFRESH] start, checking up to %d mods", workerRefreshMax)
	beginPhase("refresh", "检测更新", workerRefreshMax)
	updateProgress(func(p *WorkerProgress) { p.Platform = "" })

	// 两个来源一起检查（排除 trans:mod:body:... 那些正文键），last_checked_at 最小的优先，保证轮转。
	rows, err := db.Query(`
		SELECT key FROM translations
		WHERE key LIKE 'trans:mod:%:%:zh' AND key NOT LIKE 'trans:mod:body:%'
		ORDER BY last_checked_at ASC
		LIMIT ?
	`, workerRefreshMax)
	if err != nil {
		wlog("[REFRESH] query failed: %v", err)
		return
	}
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err == nil {
			keys = append(keys, k)
		}
	}
	rows.Close()

	wlog("[REFRESH] %d mods to check", len(keys))

	beginPhase("refresh", "翻译中", len(keys))
	updateProgress(func(p *WorkerProgress) {
		p.OK = 0
		p.Fail = 0
		p.Recent = []string{}
	})

	var okCount, failCount, changedCount atomic.Int64
	runJobsConcurrent(keys, func(key string) {
		_, platform, modID, lang := parseCacheKey(key)
		if modID == "" {
			return
		}
		if lang == "" {
			lang = "zh"
		}
		ref := modRef{Platform: platform, ID: modID}

		updateProgress(func(p *WorkerProgress) {
			p.ModID = modID
			p.Platform = platform
		})

		entry, ok := cacheGetEntry(key)
		if !ok {
			return
		}

		_, _, latestUpdated, err := fetchModFullByPlatform(platform, modID)
		if err != nil {
			wlog("[REFRESH] %s %s fetch failed: %v", platform, modID, err)
			markChecked(key)
			failCount.Add(1)
			incTodayFail()
			updateProgress(func(p *WorkerProgress) {
				p.Fail++
				p.Recent = appendRecent(p.Recent, fmt.Sprintf("✗ %s (fetch)", modID))
			})
			return
		}

		markChecked(key)

		if entry.Updated == latestUpdated {
			okCount.Add(1)
			updateProgress(func(p *WorkerProgress) {
				p.OK++
				p.Recent = appendRecent(p.Recent, fmt.Sprintf("· %s (unchanged)", modID))
			})
			return
		}

		wlog("[REFRESH] %s %s updated, re-translating", platform, modID)
		cacheDelete(key)

		if err := workerTranslateMod(ref, lang); err != nil {
			wlog("[REFRESH] %s ✗ %v", modID, err)
			failCount.Add(1)
			incTodayFail()
			updateProgress(func(p *WorkerProgress) {
				p.Fail++
				p.Recent = appendRecent(p.Recent, fmt.Sprintf("✗ %s (%s)", modID, shortErr(err)))
			})
		} else {
			changedCount.Add(1)
			okCount.Add(1)
			incTodayOk()
			updateProgress(func(p *WorkerProgress) {
				p.OK++
				p.Recent = appendRecent(p.Recent, fmt.Sprintf("↻ %s (updated)", modID))
			})
		}
	})

	if shouldStop() {
		updateProgress(func(p *WorkerProgress) { p.Status = "stopped" })
	} else {
		updateProgress(func(p *WorkerProgress) { p.Status = "done"; p.ModID = "" })
	}
	if redisAlive {
		rdb.Del(ctx, workerPendingKey)
		rdb.Set(ctx, workerLastRunKey, time.Now().Unix(), 0)
	}

	wlog("[REFRESH] finished: checked=%d ok=%d fail=%d changed=%d",
		len(keys), okCount.Load(), failCount.Load(), changedCount.Load())
}

// cacheFmtMarkdown 缓存条目的格式版本：3 = 正文经 Markdown 转换器产出（含图片保留）。
// 版本历史：2 = 首版 Markdown（图片被丢掉）；3 = 图片以 ![alt](src) 保留。
// 写缓存时带上这个标记，将来再升级正文格式时可以据此找出"还没跟上"的旧条目。
const cacheFmtMarkdown = 3

// ============== 正文失败回访 ==============

// markBodyPending 记录一个正文翻译失败的项目，供本轮预热结束前回访补翻。
// 键是 modRef 的字符串形式（"<platform>:<id>"）；历史条目是裸 modID，解析时按 Modrinth 处理。
func markBodyPending(ref modRef) {
	if !redisAlive {
		return
	}
	rdb.HIncrBy(ctx, workerBodyPendingKey, ref.String(), 1)
	rdb.Expire(ctx, workerBodyPendingKey, 30*24*time.Hour)
}

// clearBodyPending 正文补齐后从回访表移除。
func clearBodyPending(ref modRef) {
	if !redisAlive {
		return
	}
	rdb.HDel(ctx, workerBodyPendingKey, ref.String())
}

// takeBodyPending 取出待回访的 mod（排序保证稳定）；连续失败到上限的直接丢弃并记日志。
func takeBodyPending(limit int) []string {
	if !redisAlive {
		return nil
	}
	all, err := rdb.HGetAll(ctx, workerBodyPendingKey).Result()
	if err != nil || len(all) == 0 {
		return nil
	}
	out := make([]string, 0, len(all))
	dropped := 0
	for id, v := range all {
		n, _ := strconv.Atoi(v)
		if n >= workerBodyPendingMaxAttempt {
			rdb.HDel(ctx, workerBodyPendingKey, id)
			dropped++
			continue
		}
		out = append(out, id)
	}
	if dropped > 0 {
		wlog("[BODY-RETRY] 放弃 %d 个连续失败 %d 轮的正文", dropped, workerBodyPendingMaxAttempt)
	}
	sort.Strings(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// bodyRetryParallelism 正文回访的并发数（2~3）。
// 实测全池（7）跑回访时 96 个里失败 80 个：这些正文本来就是"翻失败过"的硬骨头，
// 一起打上去只会集体撞账号限速，降并发反而总吞吐更高。
func bodyRetryParallelism() int {
	n := workerParallelism() / 3
	if n < 2 {
		n = 2
	}
	if n > 3 {
		n = 3
	}
	return n
}

// retryBodyPending 回访一批正文失败的 mod。
// 必须更新进度：回访期间 current/ok 若不动，面板按增量算的速度会显示成 0（看着像卡死），
// 而缓存其实一直在涨 —— 这个阶段本身就是在补正文，属于真实工作量。
func retryBodyPending(pending []string) {
	refs := make([]modRef, 0, len(pending))
	for _, s := range pending {
		refs = append(refs, parseModRef(s))
	}

	var doneCount atomic.Int64
	updateProgress(func(p *WorkerProgress) {
		p.Type = "body-retry"
		p.Phase = "正文补翻回访"
		p.Status = "running"
		p.Current = 0
		p.OK = 0
		p.Fail = 0
		p.Total = len(refs)
		p.ModID = ""
		// 回访队列是跨来源的，这里没有"当前来源"可言，清掉免得显示上一批的来源
		p.Platform = ""
	})
	// 回访并发刻意压低（见 bodyRetryParallelism）：回访对象都是"翻失败过"的难正文，
	// 用全池并发只会一起撞账号限速，反而补得更慢。
	runJobsConcurrentN(refs, bodyRetryParallelism(), func(ref modRef) {
		if shouldStop() {
			return
		}
		updateProgress(func(p *WorkerProgress) { p.ModID = ref.ID })
		_ = workerTranslateMod(ref, "zh")
		// 只有正文真的补上了才算成功（正文失败时 workerTranslateMod 也返回 nil）
		ok := cacheHasText(modBodyCacheKey(ref.Platform, ref.ID, "zh"))
		if ok {
			incTodayBodyOk()
		}
		n := int(doneCount.Add(1))
		updateProgress(func(p *WorkerProgress) {
			p.Current = n
			if ok {
				p.OK++
				p.Recent = appendRecent(p.Recent, fmt.Sprintf("✓ %s（补正文）", ref.ID))
			} else {
				p.Fail++
				p.Recent = appendRecent(p.Recent, fmt.Sprintf("✗ %s（补正文失败）", ref.ID))
			}
		})
	})
}

// ============== 单个 mod 翻译 ==============

func workerTranslateMod(ref modRef, lang string) error {
	modID := ref.ID
	withBody := workerBodyCacheEnabled()
	for attempt := 0; attempt < workerRetryCount; attempt++ {
		if attempt > 0 {
			wlog("[RETRY] %s %s attempt %d/%d in %v", ref.Platform, modID, attempt+1, workerRetryCount, workerRetryDelay)
			time.Sleep(workerRetryDelay)
		}

		attemptStart := time.Now()
		// 一次拉取同时拿到描述和正文：开启正文缓存时不必再请求一次原站
		desc, body, updated, err := fetchModFullByPlatform(ref.Platform, modID)
		tFetch := time.Since(attemptStart)
		if err != nil {
			wlog("[WORKER] %s %s attempt %d fetch failed: %v", ref.Platform, modID, attempt+1, err)
			continue
		}

		// 只补正文的场景（该 mod 的描述早就缓存好了）不必再翻一遍描述，省掉每个 mod 一次调用
		needDesc := true
		if withBody {
			if entry, ok := cacheGetEntry(modCacheKey(ref.Platform, modID, lang)); ok && entry.Text != "" {
				needDesc = false
			}
		}

		var tDesc, tBody time.Duration
		if needDesc {
			// 描述也按 body_share 权重分摊到各 provider：否则所有 mod 的描述都压给链首那一个账号，
			// 链首一旦撞账号级限速，正文分块就会被推着走完整条兜底链，整批被拖慢。
			descSteps := bodyChain(getCfg(), modID)
			descCall := func(text, lg string) (string, error) {
				return callChainIn(getCfg(), text, lg, descSteps, true, "desc")
			}
			descStart := time.Now()
			_, err = translateAndCacheDesc(ref.Platform, modID, lang, desc, updated, descCall)
			tDesc = time.Since(descStart)
			if err != nil {
				// 描述为空说明原站确实没有内容，重试没有意义。
				if errors.Is(err, errEmptyDesc) {
					wlog("[WORKER] %s skip: %v", modID, err)
					return err
				}
				// 内容审核拒掉的描述：重试同一段文本必然再被拒，直接结束（不浪费 3 次重试）
				if errors.Is(err, errContentRejected) {
					wlog("[WORKER] %s desc rejected by content filter, skip retry", modID)
					return err
				}
				wlog("[WORKER] %s attempt %d desc failed: %v", modID, attempt+1, err)
				continue
			}
		}

		// 正文是附加项：失败只记日志，不影响该 mod 的预热结果，也不因此重试整单
		if withBody {
			bodyStart := time.Now()
			if body == "" {
				wlog("[WORKER] %s %s body skipped: empty body", ref.Platform, modID)
			} else if _, berr := translateAndCacheBody(ref.Platform, modID, lang, body, updated, getCfg(), true); berr != nil {
				if errors.Is(berr, errContentRejected) {
					// 内容审核拦下的正文：不进回访队列，否则每轮回访都要把它整篇白烧一遍
					wlog("[WORKER] %s body skipped: content rejected", modID)
					clearBodyPending(ref)
				} else {
					wlog("[WORKER] %s body failed: %v", modID, berr)
					markBodyPending(ref)
				}
			} else {
				clearBodyPending(ref)
			}
			tBody = time.Since(bodyStart)
		}
		wlog("[TIMING] %s %s fetch=%dms desc=%dms body=%dms total=%dms", ref.Platform, modID,
			tFetch.Milliseconds(), tDesc.Milliseconds(), tBody.Milliseconds(), time.Since(attemptStart).Milliseconds())
		return nil
	}

	return fmt.Errorf("after %d attempts", workerRetryCount)
}

// ============== 排行榜名单拉取（按来源）==============

// fetchTopModsFrom 按来源拉排行榜名单：从 offset 开始取 limit 个（按总下载量倒序）。
// 返回统一的 modRef，后面的翻译/回访/cursor 推进都不再关心是哪个平台。
func fetchTopModsFrom(platform string, limit, offset int) ([]modRef, error) {
	if platform == platformModrinth {
		return fetchModrinthTopMods(limit, offset)
	}
	return nil, fmt.Errorf("unsupported platform: %s", platform)
}

// fetchModrinthTopMods 从 offset 开始拉 limit 个 Modrinth 项目（index=downloads）。
func fetchModrinthTopMods(limit, offset int) ([]modRef, error) {
	const pageSize = 100
	var all []modRef
	seen := make(map[string]struct{}, pageSize*2)

	for pageOffset := offset; pageOffset < offset+limit; pageOffset += pageSize {
		pageSizeThis := pageSize
		if remaining := (offset + limit) - pageOffset; remaining < pageSize {
			pageSizeThis = remaining
		}
		url := fmt.Sprintf(
			"https://api.modrinth.com/v2/search?limit=%d&offset=%d&index=downloads",
			pageSizeThis, pageOffset,
		)
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return all, err
		}
		req.Header.Set("User-Agent", "trans-cache-worker/1.0")

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return all, err
		}

		var result struct {
			Hits []struct {
				Slug string `json:"slug"`
			} `json:"hits"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			return all, err
		}
		resp.Body.Close()

		// 拉名单期间滚动更新进度：面板的进度条在这一两分钟里必须能看出在动
		fetchProgress("拉取待预热名单（Modrinth）", len(all)+len(result.Hits), offset+limit)

		if len(result.Hits) == 0 {
			wlog("[PREHEAT] no more hits at offset=%d", pageOffset)
			break
		}
		for _, h := range result.Hits {
			if h.Slug == "" {
				continue
			}
			// Modrinth 的 index=downloads 分页跑在活索引上：翻页期间排序会漂移，
			// 同一个 mod 可能出现在两个 offset 上。不去重就会把同一个 mod 放进任务列表两次，
			// 第二次启动时第一次还在飞、缓存里还没有结果，于是白跑一遍（实测约 15%~20% 重复）。
			if _, dup := seen[h.Slug]; dup {
				continue
			}
			seen[h.Slug] = struct{}{}
			all = append(all, modRef{Platform: platformModrinth, ID: h.Slug})
		}
		wlog("[PREHEAT] fetched page offset=%d got=%d total=%d", pageOffset, len(result.Hits), len(all))
		time.Sleep(1 * time.Second)
	}

	return all, nil
}

// ============== 工具 ==============

func markChecked(key string) {
	_, _ = db.Exec(`UPDATE translations SET last_checked_at = ? WHERE key = ?`,
		time.Now().Unix(), key)
}

func incTodayOk() {
	if !redisAlive {
		return
	}
	today := time.Now().Format("2006-01-02")
	key := fmt.Sprintf("%s:%s", workerTodayOkKey, today)
	rdb.Incr(ctx, key)
	rdb.Expire(ctx, key, 48*time.Hour)
}

func incTodayFail() {
	if !redisAlive {
		return
	}
	today := time.Now().Format("2006-01-02")
	key := fmt.Sprintf("%s:%s", workerTodayFailKey, today)
	rdb.Incr(ctx, key)
	rdb.Expire(ctx, key, 48*time.Hour)
}

func getTodayStats() (int64, int64) {
	if !redisAlive {
		return 0, 0
	}
	today := time.Now().Format("2006-01-02")
	ok, _ := rdb.Get(ctx, fmt.Sprintf("%s:%s", workerTodayOkKey, today)).Int64()
	fail, _ := rdb.Get(ctx, fmt.Sprintf("%s:%s", workerTodayFailKey, today)).Int64()
	return ok, fail
}

// incTodayBodyOk 记录"今天补成功的正文篇数"。
// 单独计数：回访补正文的 mod 描述早就缓存好了，不算"新翻的 mod"（worker:today:ok 不动），
// 但它是最主要的工作量 —— 状态页要能看出来后台在干活，否则会像卡死。
func incTodayBodyOk() {
	if !redisAlive {
		return
	}
	today := time.Now().Format("2006-01-02")
	key := fmt.Sprintf("%s:%s", workerTodayBodyOkKey, today)
	rdb.Incr(ctx, key)
	rdb.Expire(ctx, key, 48*time.Hour)
}

// getTodayBodyOk 今日补成功的正文篇数。
func getTodayBodyOk() int64 {
	if !redisAlive {
		return 0
	}
	today := time.Now().Format("2006-01-02")
	n, _ := rdb.Get(ctx, fmt.Sprintf("%s:%s", workerTodayBodyOkKey, today)).Int64()
	return n
}

func getProgressOK() int {
	p := getWorkerProgress()
	return p.OK
}

func getProgressFail() int {
	p := getWorkerProgress()
	return p.Fail
}

// preheatState 各来源的预热开关与游标快照（状态接口与开关接口共用）。
func preheatState() map[string]map[string]interface{} {
	out := map[string]map[string]interface{}{}
	for _, p := range supportedPlatforms() {
		out[p] = map[string]interface{}{
			"enabled": preheatPlatformEnabled(p),
			"cursor":  getPreheatCursorFor(p),
			"step":    getPreheatStepFor(p),
		}
	}
	return out
}

func appendRecent(arr []string, item string) []string {
	arr = append(arr, item)
	if len(arr) > workerRecentMax {
		arr = arr[len(arr)-workerRecentMax:]
	}
	return arr
}

func shortErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 60 {
		s = s[:60] + "..."
	}
	return s
}

// ============== HTTP handlers ==============

func handleWorkerToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Enabled         *bool `json:"enabled"`          // 省略表示不改动
		BodyCache       *bool `json:"body_cache"`       // 是否连正文一起翻译缓存
		PreheatModrinth *bool `json:"preheat_modrinth"` // 是否参与预热
		Autocontinue    *bool `json:"autocontinue"`     // 一批跑完是否自动续跑下一批
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.Enabled != nil {
		setWorkerEnabled(*req.Enabled)
		wlog("[TOGGLE] enabled=%v", *req.Enabled)
	}
	if req.BodyCache != nil {
		setWorkerBodyCache(*req.BodyCache)
		wlog("[TOGGLE] body_cache=%v", *req.BodyCache)
	}
	if req.PreheatModrinth != nil {
		setPreheatPlatformEnabled(platformModrinth, *req.PreheatModrinth)
		wlog("[TOGGLE] preheat modrinth=%v", *req.PreheatModrinth)
	}
	if req.Autocontinue != nil {
		setWorkerAutoContinue(*req.Autocontinue)
		wlog("[TOGGLE] autocontinue=%v", *req.Autocontinue)
	}
	writeJSON(w, map[string]interface{}{
		"enabled":      workerEnabled(),
		"body_cache":   workerBodyCacheEnabled(),
		"autocontinue": workerAutoContinue(),
		"preheat":      preheatState(),
	})
}

func handleWorkerStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !workerEnabled() {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "worker is disabled"})
		return
	}
	var req struct {
		Type     string `json:"type"`
		Platform string `json:"platform"` // 可选：只跑指定来源，不填=按面板开关跑全部启用的
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.Type != "preheat" && req.Type != "refresh" {
		http.Error(w, "type must be preheat or refresh", http.StatusBadRequest)
		return
	}
	var platforms []string
	if p := strings.TrimSpace(req.Platform); p != "" && p != "all" {
		if !platformSupported(p) {
			http.Error(w, "unsupported platform: "+p, http.StatusBadRequest)
			return
		}
		platforms = []string{p}
	}
	if err := startWorker(req.Type, platforms); err != nil {
		writeJSONStatus(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"status": "started", "type": req.Type, "platform": req.Platform})
}

func handleWorkerStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	stopWorker()
	if redisAlive {
		rdb.Del(ctx, workerPendingKey)
	}
	writeJSON(w, map[string]string{"status": "stopping"})
}

func handleWorkerStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := getWorkerProgress()
	todayOk, todayFail := getTodayStats()
	enabled := workerEnabled()

	workerMu.Lock()
	running := workerRunning
	workerMu.Unlock()

	var lastRun int64
	if redisAlive {
		lastRun, _ = rdb.Get(ctx, workerLastRunKey).Int64()
	}

	writeJSON(w, map[string]interface{}{
		"enabled":      enabled,
		"body_cache":   workerBodyCacheEnabled(),
		"autocontinue": workerAutoContinue(),
		"running":      running,
		"progress":     p,
		"today_ok":     todayOk,
		"today_fail":   todayFail,
		"last_run":     lastRun,
		"parallelism":  workerParallelism(),
		"throughput":   throughputSnapshot(),
		"preheat":      preheatState(),
		// 兼容旧字段（面板旧版本只认这两个）：始终是 Modrinth 的游标/步长
		"preheat_cursor": getPreheatCursorFor(platformModrinth),
		"preheat_step":   getPreheatStepFor(platformModrinth),
	})
}

// 预热 cursor/step 的读写（按来源；platform 缺省 = modrinth，保持旧面板可用）
func handlePreheatCursor(w http.ResponseWriter, r *http.Request) {
	platform := strings.TrimSpace(r.URL.Query().Get("platform"))
	if !platformSupported(platform) {
		platform = platformModrinth
	}

	if r.Method == http.MethodGet {
		writeJSON(w, map[string]interface{}{
			"platform": platform,
			"cursor":   getPreheatCursorFor(platform),
			"step":     getPreheatStepFor(platform),
			"preheat":  preheatState(),
		})
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			Platform string `json:"platform,omitempty"`
			Cursor   *int   `json:"cursor,omitempty"`
			Step     *int   `json:"step,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if platformSupported(req.Platform) {
			platform = req.Platform
		}
		if req.Cursor != nil {
			setPreheatCursorFor(platform, *req.Cursor)
		}
		if req.Step != nil {
			setPreheatStepFor(platform, *req.Step)
		}
		writeJSON(w, map[string]interface{}{
			"platform": platform,
			"cursor":   getPreheatCursorFor(platform),
			"step":     getPreheatStepFor(platform),
			"preheat":  preheatState(),
		})
		return
	}
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

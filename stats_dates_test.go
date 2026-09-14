package main

import (
	"errors"
	"reflect"
	"testing"
)

// TestCollectStatDatesSkipsEmptyBatches 回归测试：SCAN 中间批次为空时**不能**当成扫完。
// 已经踩过的坑：这个提前 break 让「全部」只看到最后一批命中的日期，
// 区间被算成"从今天开始"，于是「近一周/全部」的统计和「今天」完全一样，
// 看起来就像历史数据丢了（其实数据都在）。
func TestCollectStatDatesSkipsEmptyBatches(t *testing.T) {
	// 模拟：第一批空（cursor 还在走），第二批才有命中，之后又空一批
	steps := []struct {
		keys []string
		next uint64
		err  error
	}{
		{[]string{}, 7, nil}, // 空批次，但没扫完 —— 关键点
		{[]string{"stats:llm:2026-09-11:model", "stats:cache:2026-09-11:hit"}, 9, nil},
		{[]string{}, 12, nil}, // 又一批空的
		{[]string{"stats:llm:2026-09-14:model"}, 0, nil},
	}
	i := 0
	got := collectStatDates(func(cursor uint64) ([]string, uint64, error) {
		if i >= len(steps) {
			return nil, 0, nil
		}
		s := steps[i]
		i++
		return s.keys, s.next, s.err
	})
	want := []string{"2026-09-11", "2026-09-14"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("收集到的日期 = %v，期望 %v（空批次被当成了结束条件？）", got, want)
	}
}

// TestCollectStatDatesShapes 各种键形态都要能认出日期，且不误认其它数字。
func TestCollectStatDatesShapes(t *testing.T) {
	keys := []string{
		"stats:llm:2026-09-13:model",
		"stats:req:translate_mod:2026-09-12",
		"stats:uptime:2026-09-14",
		"stats:iphash:1.2.3.4",      // 不该产出日期
		"stats:llm:2026-09-13:kind", // 同一天重复出现，只能算一个
		"trans:mod:modrinth:abc:zh", // 不是 stats 键（真实扫描里不会出现，这里防御性验证）
		"stats:mods:2026-09-10",
	}
	got := collectStatDates(func(cursor uint64) ([]string, uint64, error) {
		if cursor != 0 {
			return nil, 0, nil
		}
		return keys, 0, nil
	})
	want := []string{"2026-09-10", "2026-09-12", "2026-09-13", "2026-09-14"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("日期解析 = %v，期望 %v", got, want)
	}
}

// TestCollectStatDatesRedisError 出错要安全退出（不能死循环）。
func TestCollectStatDatesRedisError(t *testing.T) {
	calls := 0
	got := collectStatDates(func(cursor uint64) ([]string, uint64, error) {
		calls++
		return nil, 3, errors.New("redis down")
	})
	if len(got) != 0 {
		t.Fatalf("出错时应返回空，得到 %v", got)
	}
	if calls != 1 {
		t.Fatalf("出错后应立刻停止，实际调用了 %d 次", calls)
	}
}

// TestResolveStatRangeAll 区间解析：有历史时 from 必须是最早那天，
// 否则「全部」会退化成「今天」。
func TestResolveStatRangeAll(t *testing.T) {
	// 这里只验证 week/month 的边界（all 依赖真实 Redis 扫描，逻辑在上面的测试里）
	from, to, label := resolveStatRange("week")
	if label != "近 7 天" || from == "" || to == "" {
		t.Fatalf("近 7 天解析异常: %s %s %s", from, to, label)
	}
	if from >= to {
		t.Fatalf("近 7 天的起点应早于终点: %s ~ %s", from, to)
	}
	_, _, l2 := resolveStatRange("")
	if l2 != "今天" {
		t.Fatalf("空区间应默认今天，得到 %s", l2)
	}
}

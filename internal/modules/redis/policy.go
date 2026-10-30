package redis

import (
	"fmt"
	"strconv"
	"strings"
)

// maxRangeBytes 是 GETRANGE 这类「字节区间」命令单次可返回的最大字节数，与响应体硬上限保持一致。
const maxRangeBytes = 64 * 1024

// clampArgs 在命令发往 Redis 之前收紧位置参数。
//
// 输出侧的条目截断和 64KB 上限只能保护模型上下文：结果已经由 Redis 完整计算并传回进程内，
// 大 list/zset 的扫描时间、网络传输和堆内存开销都已经付出去了。这里把同一组上限前移到查询侧。
//
// 优先「改写」而不是「拒绝」：模型被拒后往往会换个写法重试，钳制则一次到位；
// 改写内容会通过 notes 回显在结果里，调用方仍能看到实际执行的范围。
func clampArgs(cmd string, args []string, maxItems int) ([]string, []string) {
	if maxItems <= 0 {
		maxItems = 200
	}

	switch cmd {
	case "LRANGE", "ZREVRANGE":
		return clampIndexWindow(cmd, args, int64(maxItems), fmt.Sprintf("max_items_limit=%d", maxItems))
	case "ZRANGE":
		// ZRANGE 有两种形态：按下标（start/stop）和按分值/字典序（BYSCORE/BYLEX + LIMIT）。
		if indexToken(args, "BYSCORE") >= 0 || indexToken(args, "BYLEX") >= 0 {
			return clampLimitOption(cmd, args, maxItems)
		}
		return clampIndexWindow(cmd, args, int64(maxItems), fmt.Sprintf("max_items_limit=%d", maxItems))
	case "HSCAN", "SSCAN", "ZSCAN", "SCAN":
		return clampScanCount(cmd, args, maxItems)
	case "ZRANGEBYSCORE", "ZRANGEBYLEX":
		return clampLimitOption(cmd, args, maxItems)
	case "MGET":
		return clampKeyCount(cmd, args, maxItems)
	case "SRANDMEMBER":
		return clampRandMember(cmd, args, maxItems)
	case "GETRANGE":
		return clampIndexWindow(cmd, args, maxRangeBytes, fmt.Sprintf("max_range_bytes=%d", maxRangeBytes))
	default:
		return args, nil
	}
}

// clampIndexWindow 收紧 Redis 闭区间下标命令（LRANGE key start stop / ZRANGE key start stop ...）。
// 下标可以是负数（从尾部倒数），所以必须按区间语义改写，而不是简单比较数值大小。
func clampIndexWindow(cmd string, args []string, limit int64, label string) ([]string, []string) {
	if len(args) < 3 {
		return args, nil
	}
	start, ok1 := parseIntArg(args[1])
	stop, ok2 := parseIntArg(args[2])
	if !ok1 || !ok2 {
		return args, nil
	}

	newStart, newStop := start, stop
	switch {
	case start >= 0 && stop >= 0:
		// 两端都是绝对下标，区间长度可直接比较。
		if stop-start+1 > limit {
			newStop = start + limit - 1
		}
	case start >= 0 && stop < 0:
		// stop 为负表示「到倒数第 |stop| 个」，集合长度未知，按 start 起的定长窗口收敛。
		// 对调用方而言返回的仍是被截断后的「前 N 条」，与输出侧截断结果一致。
		newStop = start + limit - 1
	case stop < 0:
		// 两端都为负时区间长度与集合总长无关（如 -5 -1 恒为 5 条），可直接比较。
		if stop-start+1 > limit {
			newStart = stop - limit + 1
		}
	default:
		// start 为负、stop 为非负：长度取决于集合大小，收敛成以 stop 结尾的定长窗口。
		newStart = stop - limit + 1
		if newStart < 0 {
			newStart = 0
		}
	}

	if newStart == start && newStop == stop {
		return args, nil
	}

	clamped := append([]string(nil), args...)
	clamped[1] = strconv.FormatInt(newStart, 10)
	clamped[2] = strconv.FormatInt(newStop, 10)
	return clamped, []string{fmt.Sprintf("%s range %s..%s -> %d..%d (%s)", cmd, args[1], args[2], newStart, newStop, label)}
}

// clampScanCount 限制 SCAN 家族的 COUNT。COUNT 只是单次迭代的提示值，
// 但 1000000 这样的值会让 Redis 在一次调用里几乎遍历整个 keyspace。
func clampScanCount(cmd string, args []string, maxItems int) ([]string, []string) {
	idx := indexToken(args, "COUNT")
	if idx < 0 || idx+1 >= len(args) {
		return args, nil
	}
	count, ok := parseIntArg(args[idx+1])
	if !ok || count <= 0 || count <= int64(maxItems) {
		return args, nil
	}

	clamped := append([]string(nil), args...)
	clamped[idx+1] = strconv.Itoa(maxItems)
	return clamped, []string{fmt.Sprintf("%s COUNT %s -> %d (max_items_limit=%d)", cmd, args[idx+1], maxItems, maxItems)}
}

// clampLimitOption 处理带 [LIMIT offset count] 的分值/字典序区间命令。
// 没有 LIMIT 时补一个定长窗口，避免「按分值取全量」；已有时只压低 count。
func clampLimitOption(cmd string, args []string, maxItems int) ([]string, []string) {
	if idx := indexToken(args, "LIMIT"); idx >= 0 {
		if idx+2 >= len(args) {
			return args, nil // 参数不完整，交给 Redis 自己报错
		}
		count, ok := parseIntArg(args[idx+2])
		if !ok || count < 0 || count <= int64(maxItems) {
			return args, nil
		}
		clamped := append([]string(nil), args...)
		clamped[idx+2] = strconv.Itoa(maxItems)
		return clamped, []string{fmt.Sprintf("%s LIMIT count %s -> %d (max_items_limit=%d)", cmd, args[idx+2], maxItems, maxItems)}
	}

	// LIMIT 必须写在 WITHSCORES 之前。
	insertAt := len(args)
	if withScores := indexToken(args, "WITHSCORES"); withScores >= 0 {
		insertAt = withScores
	}
	clamped := make([]string, 0, len(args)+3)
	clamped = append(clamped, args[:insertAt]...)
	clamped = append(clamped, "LIMIT", "0", strconv.Itoa(maxItems))
	clamped = append(clamped, args[insertAt:]...)
	return clamped, []string{fmt.Sprintf("%s had no LIMIT -> added LIMIT 0 %d (max_items_limit=%d)", cmd, maxItems, maxItems)}
}

// clampKeyCount 限制 MGET 一次带上的 key 数量。
func clampKeyCount(cmd string, args []string, maxItems int) ([]string, []string) {
	if len(args) <= maxItems {
		return args, nil
	}
	return args[:maxItems], []string{fmt.Sprintf("%s %d keys -> %d (max_items_limit=%d)", cmd, len(args), maxItems, maxItems)}
}

// clampRandMember 限制 SRANDMEMBER 的 count（负数表示允许重复，保留符号）。
func clampRandMember(cmd string, args []string, maxItems int) ([]string, []string) {
	if len(args) < 2 {
		return args, nil
	}
	count, ok := parseIntArg(args[1])
	if !ok || count == 0 {
		return args, nil
	}

	abs := count
	if abs < 0 {
		abs = -abs
	}
	if abs <= int64(maxItems) {
		return args, nil
	}

	clamped := append([]string(nil), args...)
	if count < 0 {
		clamped[1] = strconv.Itoa(-maxItems)
	} else {
		clamped[1] = strconv.Itoa(maxItems)
	}
	return clamped, []string{fmt.Sprintf("%s count %s -> %s (max_items_limit=%d)", cmd, args[1], clamped[1], maxItems)}
}

func parseIntArg(s string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n, err == nil
}

// indexToken 返回与 token 相等（忽略大小写）的参数下标。
func indexToken(args []string, token string) int {
	for i, arg := range args {
		if strings.EqualFold(arg, token) {
			return i
		}
	}
	return -1
}

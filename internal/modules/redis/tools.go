package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/casdoor/casdoor-go-sdk/casdoorsdk"
	"github.com/redis/go-redis/v9"
)

var hardBlockedCommands = map[string]bool{
	"FLUSHALL": true, "FLUSHDB": true, "CONFIG": true, "SHUTDOWN": true,
	"SET": true, "HSET": true, "DEL": true, "LPUSH": true, "RPUSH": true,
	"LPOP": true, "RPOP": true, "SADD": true, "SREM": true, "ZADD": true, "ZREM": true,
	"EVAL": true, "EVALSHA": true, "MONITOR": true, "SELECT": true, "KEYS": true,
}

// HandleExecute 执行只读 Redis 命令
func (m *Module) HandleExecute(ctx context.Context, claims *casdoorsdk.Claims, args map[string]any) (string, error) {
	cmdName, _ := args["command"].(string)
	cmdName = strings.ToUpper(strings.TrimSpace(cmdName))
	if cmdName == "" {
		return "", fmt.Errorf("missing required parameter 'command'")
	}

	// 1. 拦截高危命令并给予友好的交互引导
	if cmdName == "KEYS" {
		return "", fmt.Errorf("the 'KEYS' command is blocked for safety. Please use 'redis_scan_keys' or the 'SCAN' command instead")
	}
	if cmdName == "SELECT" {
		return "", fmt.Errorf("manual 'SELECT' command is forbidden. Please pass the 'db' parameter in tool arguments")
	}

	// 2. 检查硬黑名单与配置白名单
	if hardBlockedCommands[cmdName] || m.blockedMap[cmdName] {
		return "", fmt.Errorf("command '%s' is strictly forbidden by security policy", cmdName)
	}
	if !m.allowedMap[cmdName] {
		return "", fmt.Errorf("command '%s' is not in the allowed read-only whitelist", cmdName)
	}

	// 3. 解析 DB 编号，默认 0
	db := 0
	if dbVal, ok := args["db"]; ok {
		if dbInt, ok := toInt(dbVal); ok {
			db = dbInt
		}
	}

	rdb, err := m.dbMgr.GetClient(db)
	if err != nil {
		return "", err
	}

	// 4. 解析命令参数，并在发往 Redis 之前按命令语义收紧上限。
	//    这一步是查询侧限制：LRANGE 0 -1 / HSCAN COUNT 1000000 这类写法如果放行，
	//    Redis 会先把完整结果算完并传回来，输出侧截断已经来不及保护实例和内存。
	rawArgs, _ := args["args"].([]any)
	cmdArgs := make([]string, 0, len(rawArgs))
	for _, arg := range rawArgs {
		cmdArgs = append(cmdArgs, fmt.Sprintf("%v", arg))
	}
	cmdArgs, clampNotes := clampArgs(cmdName, cmdArgs, m.cfg.MaxItemsLimit)

	execArgs := make([]any, 0, len(cmdArgs)+1)
	execArgs = append(execArgs, cmdName)
	for _, arg := range cmdArgs {
		execArgs = append(execArgs, arg)
	}

	// 5. 动态执行（附带超时控制）
	queryCtx, cancel := context.WithTimeout(ctx, time.Duration(m.cfg.QueryTimeoutMS)*time.Millisecond)
	defer cancel()

	val, err := rdb.Do(queryCtx, execArgs...).Result()
	if err != nil {
		if err == redis.Nil {
			return "(nil)", nil
		}
		return "", fmt.Errorf("redis execution failed: %w", err)
	}

	out, err := formatAndTruncateResult(val, m.cfg.MaxItemsLimit)
	if err != nil {
		return "", err
	}
	if len(clampNotes) > 0 {
		out += "\n\n[limited] " + strings.Join(clampNotes, "; ") +
			". The result reflects the clamped range, not the full request."
	}
	return out, nil
}

// HandleScanKeys 安全遍历匹配的 Key
func (m *Module) HandleScanKeys(ctx context.Context, claims *casdoorsdk.Claims, args map[string]any) (string, error) {
	pattern, _ := args["pattern"].(string)
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		pattern = "*"
	}

	limit := 50
	if limitVal, ok := args["limit"]; ok {
		if limitInt, ok := toInt(limitVal); ok && limitInt > 0 {
			limit = limitInt
		}
	}
	if limit > m.cfg.MaxItemsLimit {
		limit = m.cfg.MaxItemsLimit
	}

	db := 0
	if dbVal, ok := args["db"]; ok {
		if dbInt, ok := toInt(dbVal); ok {
			db = dbInt
		}
	}

	rdb, err := m.dbMgr.GetClient(db)
	if err != nil {
		return "", err
	}

	queryCtx, cancel := context.WithTimeout(ctx, time.Duration(m.cfg.QueryTimeoutMS)*time.Millisecond)
	defer cancel()

	var matchedKeys []string
	var cursor uint64
	iterations := 0
	const maxScanIterations = 50

	for {
		iterations++
		keys, nextCursor, err := rdb.Scan(queryCtx, cursor, pattern, 100).Result()
		if err != nil {
			return "", fmt.Errorf("scan error: %w", err)
		}

		matchedKeys = append(matchedKeys, keys...)
		cursor = nextCursor

		if cursor == 0 || len(matchedKeys) >= limit || iterations >= maxScanIterations {
			break
		}
	}

	truncated := false
	if len(matchedKeys) > limit {
		matchedKeys = matchedKeys[:limit]
		truncated = true
	} else if cursor != 0 && iterations >= maxScanIterations {
		truncated = true
	}

	output := map[string]any{
		"db":          db,
		"pattern":     pattern,
		"count":       len(matchedKeys),
		"truncated":   truncated,
		"keys":        matchedKeys,
	}

	res, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return "", err
	}
	return string(res), nil
}

// HandleListDBs 查看各逻辑库 Key 分布
func (m *Module) HandleListDBs(ctx context.Context, claims *casdoorsdk.Claims, args map[string]any) (string, error) {
	rdb, err := m.dbMgr.GetClient(0)
	if err != nil {
		return "", err
	}

	queryCtx, cancel := context.WithTimeout(ctx, time.Duration(m.cfg.QueryTimeoutMS)*time.Millisecond)
	defer cancel()

	info, err := rdb.Info(queryCtx, "keyspace").Result()
	if err != nil {
		return "", fmt.Errorf("failed to fetch keyspace info: %w", err)
	}

	return strings.TrimSpace(info), nil
}

const maxOutputBytes = 64 * 1024 // 64 KB 响应硬上限

func formatAndTruncateResult(val any, maxLimit int) (string, error) {
	if maxLimit <= 0 {
		maxLimit = 200
	}

	truncatedVal, itemTruncated := truncateValue(val, maxLimit)

	data, err := json.MarshalIndent(truncatedVal, "", "  ")
	var out string
	if err != nil {
		out = fmt.Sprintf("%v", truncatedVal)
	} else {
		out = string(data)
	}

	if itemTruncated {
		out += fmt.Sprintf("\n... [Output truncated: collections capped to first %d items/keys]", maxLimit)
	}

	if len(out) > maxOutputBytes {
		out = out[:maxOutputBytes] + fmt.Sprintf("\n... [Output truncated: total response size capped to %d bytes]", maxOutputBytes)
	}

	return out, nil
}

func truncateValue(val any, maxLimit int) (any, bool) {
	truncated := false

	switch v := val.(type) {
	case []any:
		if len(v) > maxLimit {
			v = v[:maxLimit]
			truncated = true
		}
		res := make([]any, len(v))
		for i, item := range v {
			child, childTrunc := truncateValue(item, maxLimit)
			res[i] = child
			if childTrunc {
				truncated = true
			}
		}
		return res, truncated

	case []string:
		if len(v) > maxLimit {
			return v[:maxLimit], true
		}
		return v, false

	case map[string]any:
		if len(v) > maxLimit {
			truncated = true
			subset := make(map[string]any, maxLimit)
			count := 0
			for k, item := range v {
				subset[k] = item
				count++
				if count >= maxLimit {
					break
				}
			}
			v = subset
		}
		res := make(map[string]any, len(v))
		for k, item := range v {
			child, childTrunc := truncateValue(item, maxLimit)
			res[k] = child
			if childTrunc {
				truncated = true
			}
		}
		return res, truncated

	case map[string]string:
		if len(v) > maxLimit {
			subset := make(map[string]string, maxLimit)
			count := 0
			for k, val := range v {
				subset[k] = val
				count++
				if count >= maxLimit {
					break
				}
			}
			return subset, true
		}
		return v, false

	case map[any]any:
		// go-redis 在 RESP3 协议下返回 map[any]any，统一转为 map[string]any 并截断
		limitCount := len(v)
		if limitCount > maxLimit {
			limitCount = maxLimit
			truncated = true
		}
		res := make(map[string]any, limitCount)
		count := 0
		for k, item := range v {
			if count >= maxLimit {
				break
			}
			child, childTrunc := truncateValue(item, maxLimit)
			res[fmt.Sprintf("%v", k)] = child
			if childTrunc {
				truncated = true
			}
			count++
		}
		return res, truncated

	case string:
		if len(v) > maxOutputBytes {
			return v[:maxOutputBytes] + "... [string value truncated]", true
		}
		return v, false

	default:
		return val, false
	}
}

func toInt(val any) (int, bool) {
	switch v := val.(type) {
	case int:
		return v, true
	case float64:
		return int(v), true
	case int64:
		return int(v), true
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n, true
		}
		return 0, false
	default:
		return 0, false
	}
}

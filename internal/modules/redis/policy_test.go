package redis

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestClampIndexWindow(t *testing.T) {
	cases := []struct {
		name     string
		cmd      string
		args     []string
		limit    int
		want     []string
		wantNote bool
	}{
		{
			name:     "LRANGE 0 -1 收敛为前 N 条",
			cmd:      "LRANGE",
			args:     []string{"big_list", "0", "-1"},
			limit:    200,
			want:     []string{"big_list", "0", "199"},
			wantNote: true,
		},
		{
			name:     "ZREVRANGE 0 -1 同样收敛",
			cmd:      "ZREVRANGE",
			args:     []string{"board", "0", "-1"},
			limit:    200,
			want:     []string{"board", "0", "199"},
			wantNote: true,
		},
		{
			name:     "绝对下标超上界",
			cmd:      "LRANGE",
			args:     []string{"l", "50", "1000000"},
			limit:    200,
			want:     []string{"l", "50", "249"},
			wantNote: true,
		},
		{
			name:     "区间未超限时不改写",
			cmd:      "LRANGE",
			args:     []string{"l", "0", "49"},
			limit:    200,
			want:     []string{"l", "0", "49"},
			wantNote: false,
		},
		{
			name:     "两端都为负且跨度合法时不改写",
			cmd:      "LRANGE",
			args:     []string{"l", "-5", "-1"},
			limit:    200,
			want:     []string{"l", "-5", "-1"},
			wantNote: false,
		},
		{
			name:     "两端都为负且跨度超限时收敛尾部窗口",
			cmd:      "LRANGE",
			args:     []string{"l", "-1000000", "-1"},
			limit:    200,
			want:     []string{"l", "-200", "-1"},
			wantNote: true,
		},
		{
			name:     "负 start 正 stop 收敛成定长窗口",
			cmd:      "LRANGE",
			args:     []string{"l", "-1000000", "5000"},
			limit:    200,
			want:     []string{"l", "4801", "5000"},
			wantNote: true,
		},
		{
			name:     "参数不足时原样返回",
			cmd:      "LRANGE",
			args:     []string{"l", "0"},
			limit:    200,
			want:     []string{"l", "0"},
			wantNote: false,
		},
		{
			name:     "非数字下标交给 Redis 报错",
			cmd:      "LRANGE",
			args:     []string{"l", "a", "b"},
			limit:    200,
			want:     []string{"l", "a", "b"},
			wantNote: false,
		},
		{
			name:     "GETRANGE 按字节上限收敛",
			cmd:      "GETRANGE",
			args:     []string{"blob", "0", "-1"},
			limit:    0, // 使用 maxRangeBytes
			want:     []string{"blob", "0", "65535"},
			wantNote: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := append([]string(nil), tc.args...)
			got, notes := clampArgs(tc.cmd, tc.args, tc.limit)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("args = %v, want %v", got, tc.want)
			}
			if tc.wantNote && len(notes) == 0 {
				t.Fatalf("expected a clamp note, got none")
			}
			if !tc.wantNote && len(notes) != 0 {
				t.Fatalf("unexpected clamp note: %v", notes)
			}
			// 调用方的切片不能被就地改写
			if !reflect.DeepEqual(tc.args, original) {
				t.Fatalf("input args were mutated in place: %v -> %v", original, tc.args)
			}
		})
	}
}

func TestClampScanCount(t *testing.T) {
	cases := []struct {
		cmd      string
		args     []string
		want     []string
		wantNote bool
	}{
		{"HSCAN", []string{"h", "0", "COUNT", "1000000"}, []string{"h", "0", "COUNT", "200"}, true},
		{"SCAN", []string{"0", "MATCH", "user:*", "COUNT", "5000"}, []string{"0", "MATCH", "user:*", "COUNT", "200"}, true},
		{"SSCAN", []string{"s", "0", "count", "100"}, []string{"s", "0", "count", "100"}, false},
		{"ZSCAN", []string{"z", "0"}, []string{"z", "0"}, false},
		{"SCAN", []string{"0", "COUNT"}, []string{"0", "COUNT"}, false},
	}

	for _, tc := range cases {
		got, notes := clampArgs(tc.cmd, tc.args, 200)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s %v = %v, want %v", tc.cmd, tc.args, got, tc.want)
		}
		if tc.wantNote != (len(notes) > 0) {
			t.Errorf("%s %v: notes = %v, wantNote=%v", tc.cmd, tc.args, notes, tc.wantNote)
		}
	}
}

func TestClampLimitOption(t *testing.T) {
	cases := []struct {
		cmd      string
		args     []string
		want     []string
		wantNote bool
	}{
		{
			cmd:      "ZRANGEBYSCORE",
			args:     []string{"board", "-inf", "+inf"},
			want:     []string{"board", "-inf", "+inf", "LIMIT", "0", "200"},
			wantNote: true,
		},
		{
			cmd:      "ZRANGEBYSCORE",
			args:     []string{"board", "-inf", "+inf", "WITHSCORES"},
			want:     []string{"board", "-inf", "+inf", "LIMIT", "0", "200", "WITHSCORES"},
			wantNote: true,
		},
		{
			cmd:      "ZRANGEBYLEX",
			args:     []string{"lex", "[a", "[z", "LIMIT", "0", "100000"},
			want:     []string{"lex", "[a", "[z", "LIMIT", "0", "200"},
			wantNote: true,
		},
		{
			cmd:      "ZRANGEBYSCORE",
			args:     []string{"board", "-inf", "+inf", "LIMIT", "0", "50"},
			want:     []string{"board", "-inf", "+inf", "LIMIT", "0", "50"},
			wantNote: false,
		},
		{
			cmd:      "ZRANGE",
			args:     []string{"board", "(1", "(100", "BYSCORE"},
			want:     []string{"board", "(1", "(100", "BYSCORE", "LIMIT", "0", "200"},
			wantNote: true,
		},
	}

	for _, tc := range cases {
		got, notes := clampArgs(tc.cmd, tc.args, 200)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s %v = %v, want %v", tc.cmd, tc.args, got, tc.want)
		}
		if tc.wantNote != (len(notes) > 0) {
			t.Errorf("%s %v: notes = %v, wantNote=%v", tc.cmd, tc.args, notes, tc.wantNote)
		}
	}
}

func TestClampKeyCountAndRandMember(t *testing.T) {
	keys := make([]string, 0, 500)
	for i := 0; i < 500; i++ {
		keys = append(keys, fmt.Sprintf("k%d", i))
	}
	got, notes := clampArgs("MGET", keys, 200)
	if len(got) != 200 {
		t.Fatalf("MGET keys = %d, want 200", len(got))
	}
	if len(notes) == 0 || !strings.Contains(notes[0], "500 keys -> 200") {
		t.Fatalf("unexpected MGET note: %v", notes)
	}

	// 未超限不改写
	if got, notes = clampArgs("MGET", []string{"a", "b"}, 200); len(got) != 2 || len(notes) != 0 {
		t.Fatalf("MGET small list should be untouched, got %v notes %v", got, notes)
	}

	// SRANDMEMBER 负数保留符号
	got, notes = clampArgs("SRANDMEMBER", []string{"s", "-1000"}, 200)
	if !reflect.DeepEqual(got, []string{"s", "-200"}) || len(notes) == 0 {
		t.Fatalf("SRANDMEMBER = %v notes %v, want [-200]", got, notes)
	}
	got, notes = clampArgs("SRANDMEMBER", []string{"s", "1000"}, 200)
	if !reflect.DeepEqual(got, []string{"s", "200"}) || len(notes) == 0 {
		t.Fatalf("SRANDMEMBER = %v notes %v, want [200]", got, notes)
	}
}

func TestClampArgsLeavesOtherCommandsAlone(t *testing.T) {
	for _, cmd := range []string{"GET", "HGETALL", "TYPE", "TTL", "ZSCORE", "LRANGE_EXTRA"} {
		args := []string{"k", "0", "-1"}
		got, notes := clampArgs(cmd, args, 200)
		if !reflect.DeepEqual(got, args) || len(notes) != 0 {
			t.Errorf("%s should be untouched, got %v notes %v", cmd, got, notes)
		}
	}
}

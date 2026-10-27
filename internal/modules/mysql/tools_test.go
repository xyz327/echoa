package mysql

import (
	"strings"
	"testing"
)

func TestIsTableBlacklisted(t *testing.T) {
	blacklist := []string{
		"users_secret",
		"vault.tokens",
		"`billing`.`orders`",
		"  `sensitive`.`data`  ",
	}

	tests := []struct {
		schema   string
		table    string
		expected bool
	}{
		// 1. bare table match
		{"", "users_secret", true},
		{"other_db", "users_secret", true}, // bare table applies across any schema
		{"", "USERS_SECRET", true},         // case insensitive
		{"", "users", false},

		// 2. schema.table match
		{"vault", "tokens", true},
		{"VAULT", "TOKENS", true},
		{"other_db", "tokens", false},
		{"", "tokens", false}, // bare table doesn't match qualified blacklist rule

		// 3. quoted schema.table match
		{"billing", "orders", true},
		{"BILLING", "ORDERS", true},
		{"other_db", "orders", false},

		// 4. spaced and quoted rule
		{"sensitive", "data", true},
		{"other_db", "data", false},
	}

	for _, tc := range tests {
		got := IsTableBlacklisted(tc.schema, tc.table, blacklist)
		if got != tc.expected {
			t.Errorf("IsTableBlacklisted(%q, %q) = %v; want %v", tc.schema, tc.table, got, tc.expected)
		}
	}
}

func TestTruncateCell(t *testing.T) {
	shortStr := "hello world"
	if got := truncateCell(shortStr); got != shortStr {
		t.Errorf("expected short cell unchanged, got: %s", got)
	}

	longStr := strings.Repeat("A", maxCellBytes+500)
	got := truncateCell(longStr)
	if len(got) <= maxCellBytes {
		t.Errorf("expected truncated string with notice")
	}
	if !strings.Contains(got, "[truncated, 4596 bytes total]") {
		t.Errorf("expected truncation notice in string, got: %s", got[maxCellBytes:])
	}
}

func TestToIntHelper(t *testing.T) {
	cases := []struct {
		input    any
		expected int
		ok       bool
	}{
		{10, 10, true},
		{int64(20), 20, true},
		{float64(30), 30, true},
		{"40", 40, true},
		{" 50 ", 50, true},
		{"invalid", 0, false},
		{nil, 0, false},
	}

	for _, c := range cases {
		val, ok := toInt(c.input)
		if ok != c.ok || val != c.expected {
			t.Errorf("toInt(%v) = (%d, %v); want (%d, %v)", c.input, val, ok, c.expected, c.ok)
		}
	}
}

func TestEstimateRowsBytes(t *testing.T) {
	rows := []map[string]any{
		{"col1": "val1", "col2": 123},
		{"col1": "val2", "col2": "test"},
	}
	bytesEst := estimateRowsBytes(rows)
	if bytesEst <= 0 {
		t.Errorf("estimateRowsBytes should be > 0, got %d", bytesEst)
	}
}

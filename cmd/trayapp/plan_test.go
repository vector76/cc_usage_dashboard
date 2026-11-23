package main

import "testing"

func TestEnterpriseDetector(t *testing.T) {
	calls := 0
	file := func(v bool) func() bool { return func() bool { calls++; return v } }

	cases := []struct {
		plan  string
		file  bool
		want  bool
		reads int
	}{
		{"auto", true, true, 1},
		{"auto", false, false, 1},
		{"", true, true, 1}, // unknown value behaves as auto
		{"enterprise", false, true, 0},
		{"quota", true, false, 0},
	}
	for _, c := range cases {
		calls = 0
		if got := enterpriseDetector(c.plan, file(c.file))(); got != c.want {
			t.Errorf("plan=%q file=%v: got %v, want %v", c.plan, c.file, got, c.want)
		}
		if calls != c.reads {
			t.Errorf("plan=%q: consulted credentials %d times, want %d", c.plan, calls, c.reads)
		}
	}
}

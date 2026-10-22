package types

import "testing"

func TestLogTail(t *testing.T) {
	cases := map[string]int{"": 100, "all": 100, "0": 100, "-5": 100, "50": 50, "1000": 1000, "999999": MaxLogTail, "1&x=1": 100}
	for in, want := range cases {
		if got := LogTail(in); got != want {
			t.Errorf("LogTail(%q) = %d, want %d", in, got, want)
		}
	}
}

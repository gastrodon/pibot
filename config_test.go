package main

import "testing"

func TestSplitNonEmpty(t *testing.T) {
	if got := splitNonEmpty("", ","); got != nil {
		t.Fatalf("splitNonEmpty(\"\") = %v, want nil", got)
	}
	got := splitNonEmpty(" a , b ,, c ", ",")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("splitNonEmpty = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitNonEmpty = %v, want %v", got, want)
		}
	}
}

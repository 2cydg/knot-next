package main

import (
	"os"
	"reflect"
	"testing"
)

func TestEnvInt(t *testing.T) {
	t.Setenv("KNOT_CORE_TEST_INT", "123")
	if got := envInt("KNOT_CORE_TEST_INT", 10); got != 123 {
		t.Fatalf("envInt valid value = %d, want 123", got)
	}

	t.Setenv("KNOT_CORE_TEST_INT", "not-a-number")
	if got := envInt("KNOT_CORE_TEST_INT", 10); got != 10 {
		t.Fatalf("envInt invalid value = %d, want fallback 10", got)
	}

	if err := os.Unsetenv("KNOT_CORE_TEST_INT"); err != nil {
		t.Fatal(err)
	}
	if got := envInt("KNOT_CORE_TEST_INT", 10); got != 10 {
		t.Fatalf("envInt missing value = %d, want fallback 10", got)
	}
}

func TestSplitCSV(t *testing.T) {
	got := splitCSV(" http://a.test,https://b.test ,, ")
	want := []string{"http://a.test", "https://b.test"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splitCSV = %#v, want %#v", got, want)
	}

	if got := splitCSV(""); got != nil {
		t.Fatalf("splitCSV empty = %#v, want nil", got)
	}
}

package main

import (
	"strings"
	"testing"
)

func TestReadConcurrencyConfig(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
		valid bool
	}{
		{"", 1, true}, {"1", 1, true}, {"2", 2, true}, {"8", 8, true}, {"0", 0, false}, {"-1", 0, false}, {"9", 0, false}, {"1.5", 0, false}, {"9999999999999999999999", 0, false}, {"secret-marker", 0, false},
	} {
		cfg, err := loadConfig(env(map[string]string{"ACPLAUSE_READ_CONCURRENCY": tc.value}))
		if (err == nil) != tc.valid || tc.valid && cfg.ReadConcurrency != tc.want {
			t.Errorf("value=%q concurrency=%d err=%v", tc.value, cfg.ReadConcurrency, err)
		}
		if err != nil && (!strings.Contains(err.Error(), "ACPLAUSE_READ_CONCURRENCY") || strings.Contains(err.Error(), "secret-marker")) {
			t.Fatal("unsafe configuration error")
		}
	}
}

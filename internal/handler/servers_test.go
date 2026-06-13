package handler

import (
	"reflect"
	"strings"
	"testing"
)

func TestEnvEntries(t *testing.T) {
	t.Run("empty map yields nil", func(t *testing.T) {
		got, err := envEntries(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("flattens and sorts by key", func(t *testing.T) {
		got, err := envEntries(map[string]string{"MODE": "survival", "EULA": "TRUE", "_X": "1"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"EULA=TRUE", "MODE=survival", "_X=1"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("preserves = and empty values", func(t *testing.T) {
		got, err := envEntries(map[string]string{"JVM_OPTS": "-Xmx2G -Dk=v", "EMPTY": ""})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"EMPTY=", "JVM_OPTS=-Xmx2G -Dk=v"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("rejects invalid names", func(t *testing.T) {
		for _, bad := range []string{"1ABC", "A-B", "has space", "", "A=B"} {
			if _, err := envEntries(map[string]string{bad: "v"}); err == nil {
				t.Errorf("name %q: expected error, got nil", bad)
			}
		}
	})

	t.Run("rejects too many vars", func(t *testing.T) {
		m := make(map[string]string, maxEnvVars+1)
		for i := 0; i <= maxEnvVars; i++ {
			m["K"+strings.Repeat("X", i+1)] = "v"
		}
		if _, err := envEntries(m); err == nil {
			t.Error("expected error for exceeding maxEnvVars, got nil")
		}
	})

	t.Run("rejects oversized value", func(t *testing.T) {
		if _, err := envEntries(map[string]string{"BIG": strings.Repeat("x", maxEnvValueLen+1)}); err == nil {
			t.Error("expected error for oversized value, got nil")
		}
	})
}

package config_test

import (
	"strings"
	"testing"

	"github.com/whiskeyjimbo/engram/solov2/internal/config"
)

func TestDefaultVectorDir_NonEmpty(t *testing.T) {
	dir := config.DefaultVectorDir()
	if dir == "" {
		t.Fatal("DefaultVectorDir() returned empty string")
	}
}

func TestDefaultVectorDir_ContainsDotEngram(t *testing.T) {
	dir := config.DefaultVectorDir()
	if !strings.Contains(dir, ".engram") {
		t.Errorf("DefaultVectorDir() = %q; want path containing \".engram\"", dir)
	}
}

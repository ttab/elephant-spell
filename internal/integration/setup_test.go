package integration_test

import (
	"log"
	"os"
	"testing"

	"github.com/ttab/eltest"
)

// TestMain tears down the shared backing-services pool at process exit.
// Per-test cleanup runs via t.Cleanup; this is the safety net.
func TestMain(m *testing.M) {
	code := m.Run()

	if err := eltest.PurgeBackingServices(); err != nil {
		log.Printf("purge backing services: %v", err)
	}

	os.Exit(code)
}

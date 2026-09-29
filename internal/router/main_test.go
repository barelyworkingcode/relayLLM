package router

import (
	"os"
	"relayllm/internal/testutil"
	"testing"
)

// The splash tests re-execute this test binary as a fake splash wrapper and
// its native child; this hands those roles off before any test runs.
func TestMain(m *testing.M) {
	testutil.RunFakeSplashIfRequested()
	os.Exit(m.Run())
}

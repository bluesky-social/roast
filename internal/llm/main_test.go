package llm

import (
	"os"
	"strings"
	"testing"

	"github.com/bluesky-social/roast/internal/testutil"
)

// liveEnv holds the opt-in ROAST_LIVE_* settings. They are read before the
// ambient environment is cleared, so no other test can see them.
var liveEnv = map[string]string{}

func TestMain(m *testing.M) {
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "ROAST_LIVE_") {
			liveEnv[name] = value
		}
	}
	testutil.ClearAmbientEnvironment()
	os.Exit(m.Run())
}

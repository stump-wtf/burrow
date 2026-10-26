package store

import (
	"testing"
	"time"
)

// SetMigrateLockTimeout shortens a boot's lock wait for one test, so the test
// of a boot that cannot get its lock does not sit out the production timeout.
func SetMigrateLockTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := migrateLockTimeout
	migrateLockTimeout = d
	t.Cleanup(func() { migrateLockTimeout = old })
}

// ChangedModelProviders is the unexported comparator, exported so a test can
// pin down which rows a save sees as changed.
var ChangedModelProviders = changedModelProviders

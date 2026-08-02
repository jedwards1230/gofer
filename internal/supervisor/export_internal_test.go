package supervisor

// SetWatchSeedTestHook installs f as [watchSeedTestHook] and returns a function
// that clears it, for the external supervisor_test package (which owns the fake-
// session harness every roster test is built on, so the test that needs this
// seam cannot live in this package).
func SetWatchSeedTestHook(f func()) func() {
	watchSeedTestHook = f
	return func() { watchSeedTestHook = nil }
}

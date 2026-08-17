package observability

// ResetForTest puts the package back in its never-initialised state. Init
// touches process-wide things — the Sentry hub, logrus's hooks — so a test
// that enables reporting has to be able to disable it again, or it reports the
// next test's log lines.
func ResetForTest() { enabled = false }

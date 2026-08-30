package observability

// ResetForTest puts the package back in its never-initialised state. Init
// touches process-wide things — the Sentry hub, logrus's hooks — so a test
// that enables reporting has to be able to disable it again, or it reports the
// next test's log lines.
func ResetForTest() { enabled = false }

// SizeBucketForTest exposes the bucketing rule, which is a closed set the
// tests pin: a tag that grew a value per request would be the cardinality
// problem it exists to avoid.
func SizeBucketForTest(n int64) string { return sizeBucket(n) }

package vault

// SetAfterFirstInitRecord installs hook and returns a func restoring the previous one.
func SetAfterFirstInitRecord(hook func()) (restore func()) {
	prev := afterFirstInitRecord
	afterFirstInitRecord = hook
	return func() { afterFirstInitRecord = prev }
}

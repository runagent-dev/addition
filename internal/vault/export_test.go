package vault

import "time"

// SetAfterFirstInitRecord installs hook and returns a func restoring the previous one.
func SetAfterFirstInitRecord(hook func()) (restore func()) {
	prev := afterFirstInitRecord
	afterFirstInitRecord = hook
	return func() { afterFirstInitRecord = prev }
}

// SetBeforeWriterLock installs hook and returns a func restoring the previous one.
func SetBeforeWriterLock(hook func()) (restore func()) {
	prev := beforeWriterLock
	beforeWriterLock = hook
	return func() { beforeWriterLock = prev }
}

// SetBusyTimeout makes databases opened from now on wait d for the writer lock, and returns a func restoring the previous wait.
func SetBusyTimeout(d time.Duration) (restore func()) {
	prev := busyTimeout
	busyTimeout = d
	return func() { busyTimeout = prev }
}

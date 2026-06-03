// Package util holds small helpers shared across packages. Keep this
// package free of project-specific types so anything can import it.
package util

import "time"

// HiddifyNullDateTime is the sentinel Hiddify returns for "never connected"
// users in last_online / last_reset_time. The pool's cleanup tooling
// distinguishes this exact string from real timestamps — preserve it.
const HiddifyNullDateTime = "0001-01-01 00:00:00"

// HiddifyTime formats t (must be UTC) in Hiddify's "YYYY-MM-DD HH:MM:SS"
// shape. If t is the zero value, returns the never-connected sentinel.
func HiddifyTime(t time.Time) string {
	if t.IsZero() {
		return HiddifyNullDateTime
	}
	return t.UTC().Format("2006-01-02 15:04:05")
}

// HiddifyTimePtr is HiddifyTime with the pointer form most DB rows use.
// Nil pointer → never-connected sentinel.
func HiddifyTimePtr(t *time.Time) string {
	if t == nil {
		return HiddifyNullDateTime
	}
	return HiddifyTime(*t)
}

// HiddifyDate formats t as "YYYY-MM-DD" UTC (Hiddify's start_date shape).
// Nil pointer → JSON null at the boundary; callers using this should
// branch on nil themselves rather than relying on a sentinel string.
func HiddifyDate(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// BytesToGB converts byte counters to Hiddify's GB float (base 1024).
func BytesToGB(b int64) float64 {
	if b <= 0 {
		return 0
	}
	return float64(b) / (1024 * 1024 * 1024)
}

// GBToBytes converts a Hiddify GB float to an integer byte count, rounded
// down. Negative or NaN inputs are clamped to 0.
func GBToBytes(gb float64) int64 {
	if gb <= 0 || gb != gb { // NaN check via self-compare
		return 0
	}
	return int64(gb * 1024 * 1024 * 1024)
}

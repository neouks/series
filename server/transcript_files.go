package server

import "strings"

// Only exact SDK logs and SERIES-derived recovery sidecars belong to task file
// staging. User backups or similarly prefixed task names remain untouched.
func isTranscriptFile(name string) bool {
	if strings.HasSuffix(name, ".jsonl") {
		return true
	}
	base, suffix, ok := strings.Cut(name, ".jsonl.series-recovery.")
	if !ok || base == "" {
		return false
	}
	switch suffix {
	case "lock", "sqlite", "sqlite-journal", "sqlite-wal", "sqlite-shm", "sqlite.corrupt":
		return true
	}
	return false
}

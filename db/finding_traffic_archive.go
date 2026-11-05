package db

import (
	"context"
	"encoding/json"
)

func ArchiveEvidenceSnapshots(snapshot *TaskArchiveSnapshot) ([]TrafficEvidenceSnapshot, error) {
	var out []TrafficEvidenceSnapshot
	err := ForEachArchiveEvidenceSnapshot(context.Background(), snapshot, nil, func(v TrafficEvidenceSnapshot) error { out = append(out, v); return nil })
	return out, err
}

func normalizeArchivedFindingVersions(raw json.RawMessage) (json.RawMessage, error) {
	rows, err := decodeArchiveRows(raw)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		for _, key := range []string{"evidence_version", "report_evidence_version"} {
			if row[key] == nil {
				row[key] = 0
			}
		}
	}
	return json.Marshal(rows)
}

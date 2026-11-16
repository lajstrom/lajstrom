package main

import (
	"context"
	"log/slog"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// summaryBatch is how many versions rows fillSummaries reads and writes at a time: a metadata
// layer may be up to 4 MiB.
const summaryBatch = 25

// fillSummaries fills the summary of the versions rows written before migration 0029, which
// have none, from the metadata they keep (docs/spec/index-schema.md, The summaries of the rows
// before 0029): batch rows at a time in id order, each batch written in one transaction to the
// rows whose summary is still NULL. serve runs it once at start on every replica; two at once
// write the same values. A row without metadata waits for a reindex with verify. It logs what
// it did, or why it stopped, which the next start takes up again.
func fillSummaries(ctx context.Context, st index.Store, log *slog.Logger, batch int) {
	var after int64
	read, written, withSummary := 0, 0, 0
	stopped := func(err error) {
		if ctx.Err() == nil {
			log.WarnContext(ctx, "the summaries of the versions before migration 0029 are not all filled; the next start goes on",
				"read", read, "written", written, "error", err.Error())
		}
	}
	for {
		rows, err := st.ListVersionsWithoutSummary(ctx, after, batch)
		if err != nil {
			stopped(err)
			return
		}
		if len(rows) == 0 {
			break
		}
		summaries := make(map[int64]string, len(rows))
		for _, v := range rows {
			summaries[v.ID] = profile.Summary(v.ArtifactType, v.Metadata)
			after = v.ID
		}
		n, err := st.SetVersionSummaries(ctx, summaries)
		if err != nil {
			stopped(err)
			return
		}
		read, written = read+len(rows), written+n
		for _, s := range summaries {
			if s != "" {
				withSummary++
			}
		}
	}
	if read > 0 {
		log.InfoContext(ctx, "the summaries of the versions before migration 0029 filled from their metadata",
			"read", read, "written", written, "withSummary", withSummary)
	}
}

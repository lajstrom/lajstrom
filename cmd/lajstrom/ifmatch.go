package main

import (
	"context"
	"errors"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/index"
)

// ifMatchFlag adds --if-match to cmd, a change of a repo, a user or a webhook: the ETag that a
// GET of it answered (docs/api.md, Revisions), with which the change is made only while the
// resource has that revision, as If-Match makes the API's.
func ifMatchFlag(cmd *cobra.Command, v *string) {
	cmd.Flags().StringVar(v, "if-match", "",
		`change only while the resource has this revision: the ETag a GET of it answered, "…" with its quotes`)
}

// ifMatch returns ctx with the condition of --if-match v on the row of table that what names
// (api.IfMatch, index.WithCondition), and ctx as it is when v is empty.
func ifMatch(ctx context.Context, v, table, what string) (context.Context, error) {
	if v == "" {
		return ctx, nil
	}
	cond, err := api.IfMatch(table, what, v)
	if err != nil {
		return nil, errors.New("--if-match" + strings.TrimPrefix(err.Error(), "If-Match"))
	}
	if cond == nil {
		return ctx, nil
	}
	return index.WithCondition(ctx, cond), nil
}

// ifMatched returns the refusal of --if-match alone, which says what changed and its ETag now,
// without what the services wrapped around it; any other error as it is.
func ifMatched(err error) error {
	var pe *api.PreconditionError
	if errors.As(err, &pe) {
		return pe
	}
	return err
}

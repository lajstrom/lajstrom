package probe

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Rows are the rows of the capability matrix, in the order of docs/registries/README.md: a key
// for tooling and the row's title in the README. "Upload: mount" is the probe's own row.
var Rows = []struct{ Key, Title string }{
	{"createOnPush", "Create on push"},
	{"where", "Where a repository may be created"},
	{"referrers", "Referrers API"},
	{"ociSubject", "`OCI-Subject` on a push with `subject`"},
	{"catalog", "Catalog (`_catalog`) and its limits"},
	{"listingAPI", "A listing API of its own"},
	{"tags", "Tag list order and pagination"},
	{"consistency", "Read-after-write lag"},
	{"deleteDigest", "Delete a manifest by digest"},
	{"deleteTag", "Delete by tag"},
	{"deleteRepository", "Delete a repository"},
	{"immutableTags", "Immutable tags, where the rule lives"},
	{"blobRedirect", "Blob redirect"},
	{"range", "`Range` and `Accept-Ranges`"},
	{"largestBlob", "Largest blob measured"},
	{"manifestSize", "Manifest size limit"},
	{"profile", "Lajstrom's manifest profile (`artifactType`, empty config, own media types)"},
	{"auth", "Auth"},
	{"rateLimits", "Rate limits seen"},
	{"chunkOrder", "Upload: chunk out of order"},
	{"chunkSize", "Upload: chunk size limit"},
	{"sessionStatus", "Upload: session status (`GET`)"},
	{"cancel", "Upload: cancel (`DELETE`)"},
	{"mount", "Upload: mount (`POST ?mount=&from=`)"},
	{"wrongDigest", "Upload: wrong digest, empty blob"},
	{"replica", "Upload continued by another replica"},
	{"gc", "Garbage collector and an untagged referrer (#37)"},
	{"probeFinds", "What the probe finds"},
	{"overrides", "Overrides needed"},
}

// NotMeasured is the value of a cell that needs time or a person: garbage collection, quotas,
// rate limits, the auth flavours.
const NotMeasured = "*not measured by the probe*"

// Cell is one cell of a registry's row.
type Cell struct {
	Key   string `json:"key"`
	Row   string `json:"row"`
	Value string `json:"value"`
	// Measured is false for a cell the probe does not measure, and for one it could not.
	Measured  bool        `json:"measured"`
	Exchanges []*Exchange `json:"exchanges,omitempty"`
}

// Report is what one run measured.
type Report struct {
	// Registry is registries[].name, or the URL's host for a registry named by its URL.
	Registry string `json:"registry"`
	URL      string `json:"url"`
	// Prefix is the instance prefix, registries[].prefix; Scratch is the full name prefix the
	// run worked in, below it.
	Prefix  string `json:"prefix,omitempty"`
	Scratch string `json:"scratch"`
	Date    string `json:"date"`
	// Vendor and Capabilities are what the generic provider's probe found.
	Vendor       string       `json:"vendor"`
	Capabilities Capabilities `json:"capabilities"`
	Cells        []*Cell      `json:"cells"`
	// LeftBehind lists what the run pushed and could not delete.
	LeftBehind []string `json:"leftBehind,omitempty"`
}

// Capabilities are the provider.Capabilities of the generic provider's probe under the keys of
// registries[].capabilities.
type Capabilities struct {
	CreateOnPush   bool  `json:"createOnPush"`
	Referrers      bool  `json:"referrers"`
	Catalog        bool  `json:"catalog"`
	DeleteManifest bool  `json:"deleteManifest"`
	ImmutableTags  bool  `json:"immutableTags"`
	BlobRedirect   bool  `json:"blobRedirect"`
	MaxBlobSize    int64 `json:"maxBlobSize"`
	// Consistency is strong or eventual, empty when unknown; ConsistencyBound is a duration
	// ("2s"), empty when unknown.
	Consistency      string `json:"consistency,omitempty"`
	ConsistencyBound string `json:"consistencyBound,omitempty"`
}

// Cell returns the report's cell of key, nil for an unknown key.
func (r *Report) Cell(key string) *Cell {
	for _, c := range r.Cells {
		if c.Key == key {
			return c
		}
	}
	return nil
}

// newReport returns a report with every row not measured.
func newReport() *Report {
	r := &Report{}
	for _, row := range Rows {
		r.Cells = append(r.Cells, &Cell{Key: row.Key, Row: row.Title, Value: NotMeasured})
	}
	return r
}

// WriteMarkdown writes the reports as one table of the matrix: a column per registry, a row per
// row of the README, the cells in its vocabulary; with verbose, after it the exchanges of every
// cell, per registry.
func WriteMarkdown(w io.Writer, reports []*Report, verbose bool) error {
	var b strings.Builder
	b.WriteString("|")
	sep := "| --- |"
	for _, r := range reports {
		b.WriteString(" | " + cellText(r.Registry))
		sep += " --- |"
	}
	b.WriteString(" |\n" + sep + "\n")
	for i, row := range Rows {
		b.WriteString("| " + row.Title + " |")
		for _, r := range reports {
			b.WriteString(" " + cellText(r.Cells[i].Value) + " |")
		}
		b.WriteString("\n")
	}
	for _, r := range reports {
		fmt.Fprintf(&b, "\n%s: %s, scratch prefix `%s`, %s; the provider's probe: vendor=%s.\n",
			r.Registry, r.URL, r.Scratch, r.Date, r.Vendor)
		if len(r.LeftBehind) > 0 {
			fmt.Fprintf(&b, "Left behind (the registry refused their deletes): %s.\n", strings.Join(r.LeftBehind, ", "))
		}
	}
	if verbose {
		for _, r := range reports {
			writeExchanges(&b, r)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// writeExchanges writes the exchanges of every cell of r that has some.
func writeExchanges(b *strings.Builder, r *Report) {
	fmt.Fprintf(b, "\n## %s\n", r.Registry)
	for _, c := range r.Cells {
		if len(c.Exchanges) == 0 {
			continue
		}
		fmt.Fprintf(b, "\n### %s\n\n%s\n\n```text\n", c.Row, c.Value)
		for _, e := range c.Exchanges {
			b.WriteString(e.line() + "\n")
		}
		b.WriteString("```\n")
	}
}

// WriteJSON writes the reports as a JSON array; without verbose, the exchanges are left out.
func WriteJSON(w io.Writer, reports []*Report, verbose bool) error {
	out := reports
	if !verbose {
		out = make([]*Report, len(reports))
		for i, r := range reports {
			c := *r
			c.Cells = make([]*Cell, len(r.Cells))
			for j, cell := range r.Cells {
				cc := *cell
				cc.Exchanges = nil
				c.Cells[j] = &cc
			}
			out[i] = &c
		}
	}
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	return e.Encode(out)
}

// cellText makes a value safe in a Markdown table cell.
func cellText(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "|", `\|`), "\n", " ")
}

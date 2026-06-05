package search

import "github.com/fanlv/opensearch/internal/result"

// SummarizeEnvelope builds the stdout summary used after full search output is
// written to metadata.outputPath. The summary preserves result structure and
// URLs while omitting potentially large snippets.
func SummarizeEnvelope(full *result.Envelope) *result.Envelope {
	summary := *full
	data, ok := full.Data.(Data)
	if !ok {
		return &summary
	}

	results := make([]Result, len(data.Results))
	for i, r := range data.Results {
		results[i] = Result{
			Title:                  r.Title,
			TitleTruncated:         r.TitleTruncated,
			URL:                    r.URL,
			PublishedDate:          r.PublishedDate,
			PublishedDateTruncated: r.PublishedDateTruncated,
		}
	}
	summary.Data = Data{Results: results}
	return &summary
}

package scrape

import "github.com/fanlv/opensearch/internal/result"

// SummarizeEnvelope removes large per-item content from stdout summaries while
// keeping required item identity and status fields. The full result file remains
// untouched by the output package.
func SummarizeEnvelope(full *result.Envelope) *result.Envelope {
	summary := *full
	if !full.Success {
		return &summary
	}

	data, ok := full.Data.(Data)
	if !ok {
		return &summary
	}
	results := make([]Result, len(data.Results))
	for i, item := range data.Results {
		item.Content = ""
		results[i] = item
	}
	summary.Data = Data{Results: results}
	return &summary
}

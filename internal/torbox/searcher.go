package torbox

import "context"

// Searcher is the interface that wraps free-text torrent search. The
// torbox.Client can be configured with any Searcher implementation;
// results are then cross-checked against /torrents/checkcached.
type Searcher interface {
	// Search performs a free-text search and returns normalized results.
	// The implementation is responsible for parsing whatever response its
	// backend returns (Torznab RSS, JSON, etc.) into SearchResult values.
	Search(ctx context.Context, query string) ([]SearchResult, error)
}

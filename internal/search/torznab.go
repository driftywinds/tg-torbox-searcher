// Package search provides pluggable search backends for the bot.
// Currently supports Torznab-compatible indexers (Jackett, Prowlarr, etc.).
package search

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"torbox-tg-bot/internal/torbox"
)

// ---------------------------------------------------------------------------
// Torznab RSS response types
// ---------------------------------------------------------------------------

type torznabRSS struct {
	XMLName xml.Name       `xml:"rss"`
	Channel torznabChannel `xml:"channel"`
}

type torznabChannel struct {
	Items []torznabItem `xml:"item"`
}

type torznabItem struct {
	Title       string        `xml:"title"`
	Link        string        `xml:"link"` // magnet:?xt=urn:btih:...
	Size        string        `xml:"size"` // bytes as string
	PubDate     string        `xml:"pubDate"`
	Category    string        `xml:"category"`
	Description string        `xml:"description"`
	Attrs       []torznabAttr `xml:"http://torznab.com/schemas/1.0/ attr"` // torznab:attr
}

type torznabAttr struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

// ---------------------------------------------------------------------------
// Torznab search backend
// ---------------------------------------------------------------------------

// TorznabSearcher queries a Torznab-compatible indexer (Jackett, Prowlarr,
// etc.) and returns normalized SearchResult values. Results are then
// cross-checked against TorBox's /torrents/checkcached by the caller.
//
// Jackett endpoint (aggregate):
//   GET {baseURL}/api/v2.0/indexers/all/results/torznab?apikey={key}&t=search&q=<query>
type TorznabSearcher struct {
	baseURL    string // e.g. https://jackett.drifty.win or http://localhost:9117
	apiKey     string // Jackett requires this via ?apikey= query param
	httpClient *http.Client
}

// NewTorznabSearcher creates a new searcher pointing at a Torznab-compatible
// indexer. If apiKey is empty no authentication is sent.
func NewTorznabSearcher(baseURL, apiKey string) *TorznabSearcher {
	log.Printf("[torznab] creating searcher: baseURL=%s, hasAPIKey=%v", baseURL, apiKey != "")
	return &TorznabSearcher{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// Search implements torbox.Searcher by calling the Torznab endpoint and
// parsing the resulting XML into SearchResult values.
func (s *TorznabSearcher) Search(ctx context.Context, query string) ([]torbox.SearchResult, error) {
	u, _ := url.Parse(s.baseURL + "/api/v2.0/indexers/all/results/torznab")
	q := u.Query()
	if s.apiKey != "" {
		q.Set("apikey", s.apiKey)
	}
	q.Set("t", "search")
	q.Set("q", query)
	q.Set("limit", "100")
	q.Set("offset", "0")
	q.Set("extended", "1") // include torznab:attr metadata
	u.RawQuery = q.Encode()

	sanitizedURL := u.String()
	// Mask the API key for safe logging
	if s.apiKey != "" {
		sanitizedURL = strings.ReplaceAll(sanitizedURL, s.apiKey, "***")
	}
	log.Printf("[torznab] search: query=%q", query)
	log.Printf("[torznab] GET %s", sanitizedURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("torznab request: %w", err)
	}

	start := time.Now()
	resp, err := s.httpClient.Do(req)
	elapsed := time.Since(start)

	if err != nil {
		log.Printf("[torznab] HTTP error after %.2fs: %v", elapsed.Seconds(), err)
		// Strip API key from error before returning — Go's http.Client
		// includes the full request URL in transport errors, which would
		// leak the ?apikey=... to Telegram users.
		errMsg := err.Error()
		if s.apiKey != "" {
			errMsg = strings.ReplaceAll(errMsg, s.apiKey, "***")
		}
		return nil, fmt.Errorf("torznab http: %s", errMsg)
	}
	defer resp.Body.Close()

	log.Printf("[torznab] HTTP %d after %.2fs", resp.StatusCode, elapsed.Seconds())

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[torznab] read error: %v", err)
		return nil, fmt.Errorf("torznab read: %w", err)
	}

	log.Printf("[torznab] response body: %d bytes", len(body))

	if resp.StatusCode >= 400 {
		log.Printf("[torznab] HTTP error response: %s", truncateBytes(body, 500))
		return nil, fmt.Errorf("torznab http %d: %s", resp.StatusCode, truncateBytes(body, 200))
	}

	var rss torznabRSS
	if err := xml.Unmarshal(body, &rss); err != nil {
		return nil, fmt.Errorf("torznab xml parse: %w; body=%s", err, truncateBytes(body, 300))
	}

	results := make([]torbox.SearchResult, 0, len(rss.Channel.Items))
	for _, item := range rss.Channel.Items {
		if item.Title == "" || item.Link == "" {
			continue
		}

		sr := torbox.SearchResult{
			Name:   item.Title,
			Magnet: item.Link,
		}

		// Size as bytes
		if item.Size != "" {
			if n, err := strconv.ParseInt(item.Size, 10, 64); err == nil {
				sr.Size = n
			}
		}

		// Extract hash from magnet link
		sr.Hash = extractHashFromMagnet(item.Link)

		// Parse torznab:attr tags for additional metadata
		for _, attr := range item.Attrs {
			switch strings.ToLower(attr.Name) {
			case "infohash":
				if sr.Hash == "" {
					sr.Hash = strings.ToLower(attr.Value)
				}
			case "seeders":
				if n, err := strconv.Atoi(attr.Value); err == nil {
					sr.Seeders = n
				}
			case "peers":
				if sr.Seeders == 0 {
					if n, err := strconv.Atoi(attr.Value); err == nil {
						sr.Seeders = n
					}
				}
			case "category":
				// not directly used, could map Torznab cats later
			}
		}

		// The item's category can hint at whether it's a private tracker.
		// Jackett often includes the tracker name in the category text.
		if item.Category != "" {
			sr.Tracker = item.Category
		}
		if item.Description != "" && sr.Tracker == "" {
			if !strings.Contains(item.Description, sr.Name) {
				sr.Tracker = strings.TrimSpace(item.Description)
			}
		}
		if sr.Tracker == "" {
			sr.Tracker = "Torznab"
		}

		results = append(results, sr)
	}

	// Fill in synthetic magnets for results that have a hash but no valid magnet URI.
	// Jackett sometimes returns .torrent download URLs instead of magnet links,
	// which TorBox's /torrents/createtorrent rejects as BOZO_TORRENT.
	for i := range results {
		if !strings.HasPrefix(results[i].Magnet, "magnet:") && results[i].Hash != "" {
			dn := url.QueryEscape(results[i].Name)
			results[i].Magnet = fmt.Sprintf("magnet:?xt=urn:btih:%s&dn=%s", results[i].Hash, dn)
		}
	}

	return results, nil
}

// extractHashFromMagnet pulls the BTIH from a magnet URI.
func extractHashFromMagnet(magnet string) string {
	re := regexp.MustCompile(`xt=urn:btih:([a-fA-F0-9]{32,40})`)
	m := re.FindStringSubmatch(magnet)
	if len(m) >= 2 {
		return strings.ToLower(m[1])
	}
	return ""
}

func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

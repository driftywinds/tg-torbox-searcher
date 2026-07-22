// Package torbox is a small client for the bits of the TorBox API this bot
// needs: searching, checking cache status, creating torrents, checking
// torrent status, and (best-effort, see README) uploading a finished
// download to GoFile via TorBox's Integrations API.
package torbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	apiKey        string // TorBox API key
	gofileAPIKey  string // GoFile API token (for TorBox's integration)
	apiBaseURL    string // e.g. https://api.torbox.app/v1/api
	searchBaseURL string // e.g. https://search-api.torbox.app

	// searcher is the pluggable search backend. When set, Search() delegates
	// to it instead of calling searchBaseURL. That search's results are
	// still cross-checked via /torrents/checkcached.
	searcher Searcher

	http *http.Client

	// generalLimiter guards the documented 300 req/min per-token limit
	// (kept comfortably under that).
	generalLimiter *simpleLimiter

	// uncachedCreateLimiter guards the documented 60/hour limit that only
	// applies to *uncached* torrent creation (cached creations only count
	// against the general per-minute limit).
	uncachedCreateLimiter *simpleLimiter
}

func NewClient(apiKey, gofileAPIKey, apiBaseURL, searchBaseURL string) *Client {
	return &Client{
		apiKey:        apiKey,
		gofileAPIKey:  gofileAPIKey,
		apiBaseURL:    strings.TrimRight(apiBaseURL, "/"),
		searchBaseURL: strings.TrimRight(searchBaseURL, "/"),
		http:          &http.Client{Timeout: 30 * time.Second},
		// ~4 req/sec = 240/min, safely under the documented 300/min ceiling.
		generalLimiter: newSimpleLimiter(250*time.Millisecond, 3),
		// 60/hour with a small burst allowance.
		uncachedCreateLimiter: newSimpleLimiter(time.Hour/60, 2),
	}
}

// SetSearcher replaces the default TorBox search API with a custom search
// backend. The searcher should return magnets/hashes; Search() will still
// cross-check cache status via /torrents/checkcached.
func (c *Client) SetSearcher(s Searcher) {
	c.searcher = s
}

// ---- low level helpers -----------------------------------------------------

func (c *Client) waitGeneral(ctx context.Context) error {
	return c.generalLimiter.Wait(ctx)
}

func (c *Client) doRequest(ctx context.Context, req *http.Request) (*envelope, error) {
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("parsing response (status %d): %w; body=%s", resp.StatusCode, err, truncate(body, 300))
	}
	env.RawData = body

	if resp.StatusCode >= 400 || !env.Success {
	detail := ""
	if env.Detail != nil {
		detail = fmt.Sprintf("%v", env.Detail)
	}
	if detail == "" {
		detail = string(truncate(body, 300))
	}
		errCode := ""
		if env.Error != nil {
			errCode = *env.Error
		}
		return &env, fmt.Errorf("torbox api error (status %d, code %s): %s", resp.StatusCode, errCode, detail)
	}

	return &env, nil
}

func truncate(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return append(append([]byte{}, b[:n]...), []byte("...")...)
}

// ---- search -----------------------------------------------------------------

// Search performs a free-text search using whichever backend is configured
// (the Searcher interface), then cross-checks every result's cache status
// via the documented /torrents/checkcached endpoint.
//
// If no custom Searcher is set (the default), it falls back to calling
// TorBox's own search API at searchBaseURL.
func (c *Client) Search(ctx context.Context, query string) ([]SearchResult, error) {
	var results []SearchResult
	var err error

	if c.searcher != nil {
		// Custom search backend (e.g. Zilean, Prowlarr).
		if err := c.waitGeneral(ctx); err != nil {
			return nil, err
		}
		results, err = c.searcher.Search(ctx, query)
	} else {
		// Default: TorBox's own search API.
		if err := c.waitGeneral(ctx); err != nil {
			return nil, err
		}
		u := fmt.Sprintf("%s/torrents/search/%s?metadata=true&check_cache=true",
			c.searchBaseURL, url.PathEscape(query))
		req, err2 := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err2 != nil {
			return nil, err2
		}
		env, err2 := c.doRequest(ctx, req)
		if err2 != nil {
			return nil, err2
		}
		results, err = parseSearchResults(env.Data)
	}

	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return results, nil
	}

	// Cross check real cache status using the documented endpoint.
	hashes := make([]string, 0, len(results))
	for _, r := range results {
		if r.Hash != "" {
			hashes = append(hashes, r.Hash)
		}
	}
	cached, err := c.CheckCachedBatch(ctx, hashes)
	if err == nil {
		for i := range results {
			if isCached, ok := cached[strings.ToLower(results[i].Hash)]; ok {
				results[i].Cached = isCached
			}
		}
	}
	// If the checkcached call itself failed, we just fall back to whatever
	// the search said (or false), rather than failing the whole search.

	return results, nil
}

func parseSearchResults(data interface{}) ([]SearchResult, error) {
	var rawItems []interface{}

	switch v := data.(type) {
	case []interface{}:
		rawItems = v
	case map[string]interface{}:
		for _, key := range []string{"torrents", "results", "items"} {
			if list, ok := v[key].([]interface{}); ok {
				rawItems = list
				break
			}
		}
	}

	results := make([]SearchResult, 0, len(rawItems))
	for _, raw := range rawItems {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		results = append(results, SearchResult{
			Name:    firstString(m, "raw_title", "title", "name"),
			Hash:    strings.ToLower(firstString(m, "hash", "info_hash", "infohash")),
			Size:    firstInt(m, "size", "filesize", "bytes"),
			Magnet:  firstString(m, "magnet", "magnet_link", "magnetlink"),
			Tracker: firstString(m, "tracker", "indexer", "source"),
			Private: firstBool(m, "private", "is_private"),
			Seeders: int(firstInt(m, "seeders", "seed", "last_known_seeders")),
		})
	}

	// Fill in a synthetic magnet link if the search result gave us a hash
	// but no magnet (common for some search backends).
	for i := range results {
		if results[i].Magnet == "" && results[i].Hash != "" {
			dn := url.QueryEscape(results[i].Name)
			results[i].Magnet = fmt.Sprintf("magnet:?xt=urn:btih:%s&dn=%s", results[i].Hash, dn)
		}
	}

	return results, nil
}

func firstString(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch val := v.(type) {
			case string:
				if val != "" {
					return val
				}
			case float64:
				return strconv.FormatInt(int64(val), 10)
			case int64:
				return strconv.FormatInt(val, 10)
			}
		}
	}
	return ""
}

func firstInt(m map[string]interface{}, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch n := v.(type) {
			case float64:
				return int64(n)
			case int64:
				return n
			case string:
				if parsed, err := strconv.ParseInt(n, 10, 64); err == nil {
					return parsed
				}
			}
		}
	}
	return 0
}

func firstBool(m map[string]interface{}, keys ...string) bool {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if b, ok := v.(bool); ok {
				return b
			}
		}
	}
	return false
}

// ---- cache check (documented endpoint) --------------------------------------

// CheckCachedBatch calls GET /torrents/checkcached for up to ~100 hashes at
// a time and returns a map of lowercase hash -> cached bool.
func (c *Client) CheckCachedBatch(ctx context.Context, hashes []string) (map[string]bool, error) {
	result := make(map[string]bool, len(hashes))
	if len(hashes) == 0 {
		return result, nil
	}

	const chunkSize = 100
	for start := 0; start < len(hashes); start += chunkSize {
		end := start + chunkSize
		if end > len(hashes) {
			end = len(hashes)
		}
		chunk := hashes[start:end]

		if err := c.waitGeneral(ctx); err != nil {
			return result, err
		}

		u := fmt.Sprintf("%s/torrents/checkcached?format=list&list_files=false&hash=%s",
			c.apiBaseURL, url.QueryEscape(strings.Join(chunk, ",")))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return result, err
		}

		env, err := c.doRequest(ctx, req)
		if err != nil {
			return result, err
		}

		// format=list returns a list of cached items (each with a "hash" field).
		// Anything not present in the list is simply not cached.
		for _, h := range chunk {
			result[strings.ToLower(h)] = false
		}
		if list, ok := env.Data.([]interface{}); ok {
			for _, raw := range list {
				if m, ok := raw.(map[string]interface{}); ok {
					if h := firstString(m, "hash"); h != "" {
						result[strings.ToLower(h)] = true
					}
				}
			}
		}
	}

	return result, nil
}

// ---- creating / checking torrents (documented endpoints) --------------------

// CreateTorrentFromMagnet adds a magnet link to the user's TorBox account.
// `cached` should reflect whether we believe this item is already cached
// (from search + checkcached), since that determines which rate limiter we
// consume from.
func (c *Client) CreateTorrentFromMagnet(ctx context.Context, magnet string, cached bool) (torrentID int64, hash string, err error) {
	if err := c.waitGeneral(ctx); err != nil {
		return 0, "", err
	}
	if !cached {
		if err := c.uncachedCreateLimiter.Wait(ctx); err != nil {
			return 0, "", err
		}
	}

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	if err := w.WriteField("magnet", magnet); err != nil {
		return 0, "", err
	}
	if err := w.WriteField("seed", "3"); err != nil { // don't seed, we only care about the download
		return 0, "", err
	}
	if err := w.Close(); err != nil {
		return 0, "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBaseURL+"/torrents/createtorrent", body)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	env, err := c.doRequest(ctx, req)
	if err != nil {
		// DUPLICATE_ITEM means it's already on the account - try to recover
		// the existing torrent id by looking it up via mylist below is left
		// to the caller (bot layer) since we don't have the hash handy here
		// in every case.
		return 0, "", err
	}

	m, ok := env.Data.(map[string]interface{})
	if !ok {
		return 0, "", fmt.Errorf("unexpected createtorrent response shape")
	}
	torrentID = firstInt(m, "torrent_id")
	hash = strings.ToLower(firstString(m, "hash"))
	return torrentID, hash, nil
}

// FindTorrentByHash looks through the user's torrent list for one matching
// the given hash. Useful when createtorrent returns DUPLICATE_ITEM.
func (c *Client) FindTorrentByHash(ctx context.Context, hash string) (*TorrentInfo, error) {
	if err := c.waitGeneral(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBaseURL+"/torrents/mylist?bypass_cache=true", nil)
	if err != nil {
		return nil, err
	}
	env, err := c.doRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	list, ok := env.Data.([]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected mylist response shape")
	}
	for _, raw := range list {
		b, _ := json.Marshal(raw)
		var info TorrentInfo
		if err := json.Unmarshal(b, &info); err == nil {
			if strings.EqualFold(info.Hash, hash) {
				return &info, nil
			}
		}
	}
	return nil, fmt.Errorf("no torrent found with hash %s", hash)
}

// GetTorrentByID fetches current status for a single torrent.
func (c *Client) GetTorrentByID(ctx context.Context, id int64) (*TorrentInfo, error) {
	if err := c.waitGeneral(ctx); err != nil {
		return nil, err
	}
	u := fmt.Sprintf("%s/torrents/mylist?bypass_cache=true&id=%d", c.apiBaseURL, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	env, err := c.doRequest(ctx, req)
	if err != nil {
		return nil, err
	}

	b, err := json.Marshal(env.Data)
	if err != nil {
		return nil, err
	}
	var info TorrentInfo
	// mylist?id=X can return either an object or a single-element list
	// depending on TorBox version; handle both.
	if err := json.Unmarshal(b, &info); err == nil && info.ID != 0 {
		return &info, nil
	}
	var list []TorrentInfo
	if err := json.Unmarshal(b, &list); err == nil && len(list) > 0 {
		return &list[0], nil
	}
	return nil, fmt.Errorf("torrent %d not found", id)
}

// ---- GoFile upload via TorBox Integrations API (BEST EFFORT, see README) ---
//
// The provided TorBox docs only show DELETE /integration/job/{job_id} for
// cancelling a job. They do NOT show the endpoint used to *create* a GoFile
// upload job. Based on TorBox's naming conventions elsewhere in the API
// (settings has `gofile_folder_id` / `gofile_api_key`, and there's a whole
// "Integrations" section with a job system), the calls below are a
// best-effort guess. If they don't match TorBox's real Integrations API,
// this is the place to fix it up - everything else in this bot is decoupled
// from the exact request/response shape here.

// CreateGofileJob kicks off an upload-to-GoFile job for a finished torrent
// download and returns a job id to poll.
func (c *Client) CreateGofileJob(ctx context.Context, torrentID int64) (jobID string, err error) {
	if err := c.waitGeneral(ctx); err != nil {
		return "", err
	}

	payload := map[string]interface{}{
		"type": "torrent",
		"id":   torrentID,
		"zip":  true,
	}
	if c.gofileAPIKey != "" {
		payload["gofile_token"] = c.gofileAPIKey
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBaseURL+"/integration/gofile", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	env, err := c.doRequest(ctx, req)
	if err != nil {
		return "", fmt.Errorf("creating gofile job (this endpoint is a best-effort guess, see README): %w", err)
	}

	// Log the raw response to debug the actual response shape
	log.Printf("[gofile] raw response body: %s", string(env.RawData))
	log.Printf("[gofile] parsed data: %+v (type: %T)", env.Data, env.Data)

	m, ok := env.Data.(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("unexpected gofile job creation response shape (type %T): %s", env.Data, string(env.RawData))
	}
	log.Printf("[gofile] data map keys: %v", mapKeys(m))
	jobID = firstString(m, "job_id", "id", "jobId", "upload_id")
	if jobID == "" {
		return "", fmt.Errorf("no job id in gofile job creation response; body=%s", string(env.RawData))
	}
	return jobID, nil
}

// GetJobStatus polls the status of a previously created integration job.
func (c *Client) GetJobStatus(ctx context.Context, jobID string) (*JobStatus, error) {
	if err := c.waitGeneral(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBaseURL+"/integration/job/"+url.PathEscape(jobID), nil)
	if err != nil {
		return nil, err
	}
	env, err := c.doRequest(ctx, req)
	if err != nil {
		return nil, err
	}

	b, err := json.Marshal(env.Data)
	if err != nil {
		return nil, err
	}

	// Log the raw status response for debugging
	log.Printf("[gofile] job status raw: %s", string(b))

	var status JobStatus
	if err := json.Unmarshal(b, &status); err != nil {
		return nil, fmt.Errorf("parsing job status (body=%s): %w", string(b), err)
	}
	status.ID = jobID
	log.Printf("[gofile] job %s status: %q  download_url=%q", jobID, status.Status, status.DownloadURL)
	return &status, nil
}

// mapKeys returns the keys of m as a sorted slice (for debug logging).
func mapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// CancelJob cancels/deletes an integration job. This one IS documented.
func (c *Client) CancelJob(ctx context.Context, jobID string) error {
	if err := c.waitGeneral(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.apiBaseURL+"/integration/job/"+url.PathEscape(jobID), nil)
	if err != nil {
		return err
	}
	_, err = c.doRequest(ctx, req)
	return err
}

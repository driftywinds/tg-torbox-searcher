package torbox

// envelope is TorBox's standard API response wrapper, used by (almost) every endpoint.
type envelope struct {
	Success bool            `json:"success"`
	Error   *string         `json:"error"`
	Detail  interface{}     `json:"detail"`
	Data    interface{}     `json:"data"`
	RawData []byte          `json:"-"`
}

// SearchResult is a normalized search hit, combining fields from the Search
// API with a reliable cached-status lookup done via the documented
// /torrents/checkcached endpoint.
type SearchResult struct {
	Name     string
	Hash     string
	Size     int64  // bytes
	Magnet   string // magnet link, if available
	Tracker  string
	Private  bool
	Seeders  int
	Cached   bool
}

// TorrentInfo is a subset of the fields returned by GET /torrents/mylist.
type TorrentInfo struct {
	ID               int64   `json:"id"`
	Name             string  `json:"name"`
	Hash             string  `json:"hash"`
	Size             int64   `json:"size"`
	DownloadState    string  `json:"download_state"`
	DownloadSpeed    int64   `json:"download_speed"`
	Progress         float64 `json:"progress"`
	ETA              int64   `json:"eta"`
	DownloadPresent  bool    `json:"download_present"`
	DownloadFinished bool    `json:"download_finished"`
	Cached           bool    `json:"cached"`
	Files            []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"files"`
}

// JobStatus represents the state of an async integration job (e.g. uploading
// a completed download to GoFile). NOTE: the exact shape of this response is
// an assumption - see README "Known gaps" section.
//
// ID is tagged json:"-" because TorBox returns it as a number and we already
// know it (it's the URL parameter we used to fetch status).
type JobStatus struct {
	ID          string  `json:"-"`
	Status      string  `json:"status"` // e.g. "pending", "uploading", "completed", "failed"
	DownloadURL string  `json:"download_url"`
	Progress    float64 `json:"progress"`
	Detail      string  `json:"detail"` // user-friendly status/error message from TorBox
}

func (j *JobStatus) IsDone() bool {
	switch j.Status {
	case "completed", "success", "finished", "done":
		return true
	}
	return j.DownloadURL != ""
}

func (j *JobStatus) IsFailed() bool {
	switch j.Status {
	case "failed", "error":
		return true
	}
	return false
}

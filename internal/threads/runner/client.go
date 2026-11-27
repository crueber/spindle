// Package runner implements the spindle Threads runner: the client half of the
// pass-B1 protocol (15_threads.md §3) plus the pass-B2 container executor
// (§4). It lives beside the server package so both ends of the wire share one
// set of types (threads.FetchResult, threads.Spec, …); the runner itself is a
// separate process (cmd/spindle's `runner serve` subcommand) and never imports
// the server's store or handler code.
//
// Execution model (15 §4): one container per job, services on a per-job
// network, a per-job workspace volume, checkout inside the container with the
// attempt token, each step via `docker exec`, one log frame stream per attempt.
// docker is a subprocess exactly like git (law 1: no SDK dependency).
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/threads"
)

// ProtocolVersion is the runner protocol version this client speaks
// (X-Spindle-Runner: 1).
const ProtocolVersion = "1"

// ErrUnauthorized marks a credential the server rejected: the runner stops and
// asks the operator to re-register (retrying can never fix it).
var ErrUnauthorized = errors.New("runner credential rejected")

// ErrSuperseded marks an attempt the server has already replaced (a requeue or
// a rerun): the job is not ours any more, so the runner abandons it without a
// completion write.
var ErrSuperseded = errors.New("attempt superseded")

// APIError is one non-2xx protocol answer.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	msg := strings.Join(strings.Fields(e.Message), " ")
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("%s (HTTP %d)", msg, e.Status)
}

// Client is the runner's HTTP client: one per process, safe for concurrent
// use (capacity >1 runs one fetch per worker).
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewClient builds a protocol client for base (scheme://host[:port]).
func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// RunnerOptions is the registration/declare statement (PAR-173/PAR-176): the
// runner's name, labels, capacity, fork opt-in, single-job declaration and
// declared size ceiling. It never carries the repository policy (the admin's).
type RunnerOptions struct {
	Name           string
	Labels         []string
	Capacity       int
	AllowForkRuns  bool
	Ephemeral      bool
	MaxCPUs        float64
	MaxMemoryBytes int64
}

// Register consumes a one-time registration token and returns the long-lived
// runner credential (the one response that carries a secret).
func (c *Client) Register(ctx context.Context, regToken, name string, labels []string, capacity int, allowForkRuns bool) (*threads.RegisterResult, error) {
	return c.RegisterRunner(ctx, regToken, RunnerOptions{Name: name, Labels: labels, Capacity: capacity, AllowForkRuns: allowForkRuns})
}

// RegisterRunner is Register with the full declaration.
func (c *Client) RegisterRunner(ctx context.Context, regToken string, opts RunnerOptions) (*threads.RegisterResult, error) {
	body := map[string]any{
		"token": regToken, "name": opts.Name, "labels": opts.Labels, "capacity": opts.Capacity,
		"allow_fork_runs": opts.AllowForkRuns, "ephemeral": opts.Ephemeral,
		"max_cpus": opts.MaxCPUs, "max_memory_bytes": opts.MaxMemoryBytes,
	}
	var out threads.RegisterResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/threads/runners/register", "", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Declare records the runner's version and declaration (the runner's liveness
// statement; fetching is the heartbeat).
func (c *Client) Declare(ctx context.Context, version string, labels []string, capacity int, allowForkRuns bool) (*threads.DeclareView, error) {
	return c.DeclareRunner(ctx, version, RunnerOptions{Labels: labels, Capacity: capacity, AllowForkRuns: allowForkRuns})
}

// DeclareRunner is Declare with the full declaration.
func (c *Client) DeclareRunner(ctx context.Context, version string, opts RunnerOptions) (*threads.DeclareView, error) {
	var out threads.DeclareView
	if err := c.do(ctx, http.MethodPost, "/api/v1/threads/runners/declare", c.Token,
		map[string]any{
			"version": version, "labels": opts.Labels, "capacity": opts.Capacity,
			"allow_fork_runs": opts.AllowForkRuns, "ephemeral": opts.Ephemeral,
			"max_cpus": opts.MaxCPUs, "max_memory_bytes": opts.MaxMemoryBytes,
		}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Drain asks the server to stop offering this runner new jobs while the current
// one finishes (PAR-181).
func (c *Client) Drain(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/v1/threads/runners/drain", c.Token, nil, nil)
}

// Resume undoes a drain.
func (c *Client) Resume(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/v1/threads/runners/resume", c.Token, nil, nil)
}

// Rotate issues a new credential with an overlap window; the caller writes it
// to its token file before the old one expires.
func (c *Client) Rotate(ctx context.Context) (*threads.RunnerTokenRotation, error) {
	var out threads.RunnerTokenRotation
	if err := c.do(ctx, http.MethodPost, "/api/v1/threads/runners/rotate", c.Token, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Deregister removes this runner's record and credential.
func (c *Client) Deregister(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/v1/threads/runners/deregister", c.Token, nil, nil)
}

// Fetch long-polls for one job. (nil, nil) means the poll expired (204).
func (c *Client) Fetch(ctx context.Context, wait time.Duration) (*threads.FetchResult, error) {
	secs := int(wait / time.Second)
	if secs <= 0 {
		secs = 20
	}
	path := "/api/v1/threads/runners/fetch?timeout=" + strconv.Itoa(secs)
	var out threads.FetchResult
	found, err := c.doMaybe(ctx, http.MethodPost, path, c.Token, nil, &out)
	if err != nil || !found {
		return nil, err
	}
	return &out, nil
}

// Start opens one attempt and returns its short-lived token.
func (c *Client) Start(ctx context.Context, jobRef string, attempt int) (*threads.StartView, error) {
	var out threads.StartView
	if err := c.do(ctx, http.MethodPost, attemptPath(jobRef, attempt, "start"), c.Token, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Log appends one frame (idempotent by seq). The answer carries the cancel
// signal the operator set (the busy runner's control channel).
func (c *Client) Log(ctx context.Context, token, jobRef string, attempt, seq int, rec threads.LogRecord) (*threads.LogView, error) {
	var out threads.LogView
	body := struct {
		Seq int `json:"seq"`
		threads.LogRecord
	}{Seq: seq, LogRecord: rec}
	if err := c.do(ctx, http.MethodPost, attemptPath(jobRef, attempt, "log"), token, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Complete posts the conclusion and step results.
func (c *Client) Complete(ctx context.Context, token, jobRef string, attempt int, in threads.CompleteInput) (*threads.CompleteView, error) {
	var out threads.CompleteView
	if err := c.do(ctx, http.MethodPost, attemptPath(jobRef, attempt, "complete"), token, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AckCancel acknowledges a cancellation.
func (c *Client) AckCancel(ctx context.Context, token, jobRef string, attempt int) (*threads.CompleteView, error) {
	var out threads.CompleteView
	if err := c.do(ctx, http.MethodPost, attemptPath(jobRef, attempt, "cancel"), token, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// attemptPath renders /api/v1/threads/jobs/{ref}/attempt/{n}/{op}.
func attemptPath(jobRef string, attempt int, op string) string {
	return "/api/v1/threads/jobs/" + jobRef + "/attempt/" + strconv.Itoa(attempt) + "/" + op
}

// artifactPath renders one artifact route under the attempt.
func artifactPath(jobRef string, attempt int, name string, rest ...string) string {
	base := attemptPath(jobRef, attempt, "artifacts") + "/" + neturl.PathEscape(name)
	for _, seg := range rest {
		base += "/" + seg
	}
	return base
}

// PutArtifactBlob uploads one artifact file (raw bytes, ?path= and ?size=).
func (c *Client) PutArtifactBlob(ctx context.Context, token, jobRef string, attempt int, name string, index int, filePath string, size int64, body io.Reader) (*threads.ArtifactBlobView, error) {
	path := artifactPath(jobRef, attempt, name, "blobs", strconv.Itoa(index)) +
		"?path=" + neturl.QueryEscape(filePath) + "&size=" + strconv.FormatInt(size, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Spindle-Runner", ProtocolVersion)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, apiErrorFrom(resp.StatusCode, raw)
	}
	var out threads.ArtifactBlobView
	if jerr := json.Unmarshal(raw, &out); jerr != nil {
		return nil, fmt.Errorf("decode artifact blob answer: %w", jerr)
	}
	return &out, nil
}

// FinalizeArtifact commits the artifact's manifest.
func (c *Client) FinalizeArtifact(ctx context.Context, token, jobRef string, attempt int, in threads.ArtifactFinalizeInput) (*threads.ArtifactView, error) {
	var out threads.ArtifactView
	if err := c.do(ctx, http.MethodPost, attemptPath(jobRef, attempt, "artifacts"), token, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ArtifactManifest reads one artifact's manifest (the download step).
func (c *Client) ArtifactManifest(ctx context.Context, token, jobRef string, attempt int, name string) (*threads.Artifact, error) {
	var out threads.Artifact
	if err := c.do(ctx, http.MethodGet, artifactPath(jobRef, attempt, name), token, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetArtifactBlob streams one artifact file back; the caller closes the body.
func (c *Client) GetArtifactBlob(ctx context.Context, token, jobRef string, attempt int, name string, index int) (io.ReadCloser, int64, error) {
	path := artifactPath(jobRef, attempt, name, "blobs", strconv.Itoa(index))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Spindle-Runner", ProtocolVersion)
	// The blob can be large: no whole-body read, and no client timeout on the
	// streaming client (the caller's context bounds it).
	hc := &http.Client{Timeout: 0}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, 0, apiErrorFrom(resp.StatusCode, body)
	}
	return resp.Body, resp.ContentLength, nil
}

// apiErrorFrom maps a non-2xx raw answer onto the client's error vocabulary.
func apiErrorFrom(status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: %s", ErrUnauthorized, msg)
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrSuperseded, msg)
	default:
		return &APIError{Status: status, Message: msg}
	}
}

// do issues one request with a bearer token and decodes a 2xx body.
func (c *Client) do(ctx context.Context, method, path, bearer string, in any, out any) error {
	_, err := c.doMaybe(ctx, method, path, bearer, in, out)
	return err
}

// doMaybe is do with the 204 distinction: (false, nil) means an empty body.
func (c *Client) doMaybe(ctx context.Context, method, path, bearer string, in any, out any) (bool, error) {
	var rdr io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return false, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return false, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Spindle-Runner", ProtocolVersion)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	switch {
	case resp.StatusCode == http.StatusNoContent:
		return false, nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if out == nil {
			return true, nil
		}
		if jerr := json.Unmarshal(body, out); jerr != nil {
			return false, fmt.Errorf("decode %s %s: %w", method, path, jerr)
		}
		return true, nil
	case resp.StatusCode == http.StatusUnauthorized:
		return false, fmt.Errorf("%w: %s", ErrUnauthorized, strings.TrimSpace(string(body)))
	case resp.StatusCode == http.StatusConflict:
		return false, fmt.Errorf("%w: %s", ErrSuperseded, strings.TrimSpace(string(body)))
	default:
		return false, &APIError{Status: resp.StatusCode, Message: string(body)}
	}
}

// isFatal reports an error the runner must not retry (bad credential,
// superseded attempt).
func isFatal(err error) bool {
	return errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrSuperseded)
}

// --- cache transport (PAR-095) ---------------------------------------------

// cachePath is the attempt-scoped cache sub-tree.
func cachePath(jobRef string, attempt int, rest ...string) string {
	base := attemptPath(jobRef, attempt, "cache")
	for _, seg := range rest {
		base += "/" + seg
	}
	return base
}

// CacheLookup asks the server for a restore candidate.
func (c *Client) CacheLookup(ctx context.Context, token, jobRef string, attempt int, in threads.CacheLookupInput) (*threads.CacheView, error) {
	var out threads.CacheView
	if err := c.do(ctx, http.MethodPost, cachePath(jobRef, attempt, "lookup"), token, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCacheBlob streams one cache payload back; the caller closes the body.
func (c *Client) GetCacheBlob(ctx context.Context, token, jobRef string, attempt int, sha string) (io.ReadCloser, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+cachePath(jobRef, attempt, "blobs", sha), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Spindle-Runner", ProtocolVersion)
	// A cache payload can be large: no client timeout on the streaming client.
	hc := &http.Client{Timeout: 0}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, 0, apiErrorFrom(resp.StatusCode, body)
	}
	return resp.Body, resp.ContentLength, nil
}

// PutCacheBlob uploads one content-addressed payload.
func (c *Client) PutCacheBlob(ctx context.Context, token, jobRef string, attempt int, sha string, size int64, body io.Reader) error {
	path := cachePath(jobRef, attempt, "blobs", sha) + "?size=" + strconv.FormatInt(size, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Spindle-Runner", ProtocolVersion)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return apiErrorFrom(resp.StatusCode, raw)
	}
	return nil
}

// SaveCache commits one cache entry.
func (c *Client) SaveCache(ctx context.Context, token, jobRef string, attempt int, in threads.SaveCacheInput) (*threads.CacheView, error) {
	var out threads.CacheView
	if err := c.do(ctx, http.MethodPost, cachePath(jobRef, attempt, "save"), token, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PrepareArtifact opens an upload (overwrite deletes the existing artifact
// before the blobs are streamed, PAR-097).
func (c *Client) PrepareArtifact(ctx context.Context, token, jobRef string, attempt int, name string, overwrite bool) (bool, error) {
	var out struct {
		Existed bool `json:"existed"`
	}
	body := map[string]any{"overwrite": overwrite}
	if err := c.do(ctx, http.MethodPost, artifactPath(jobRef, attempt, name, "prepare"), token, body, &out); err != nil {
		return false, err
	}
	return out.Existed, nil
}

// ArtifactList lists a run's artifacts (the download-all/pattern/ids path).
func (c *Client) ArtifactList(ctx context.Context, token, jobRef string, attempt int, runID string) ([]*threads.Artifact, error) {
	path := attemptPath(jobRef, attempt, "artifacts")
	if runID != "" {
		path += "?run_id=" + neturl.QueryEscape(runID)
	}
	var out struct {
		Artifacts []*threads.Artifact `json:"artifacts"`
	}
	if err := c.do(ctx, http.MethodGet, path, token, nil, &out); err != nil {
		return nil, err
	}
	return out.Artifacts, nil
}

// ArtifactManifestFromRun reads one artifact's manifest, optionally from another
// run of the same repository (the D-3 gate is the server's).
func (c *Client) ArtifactManifestFromRun(ctx context.Context, token, jobRef string, attempt int, runID, name string) (*threads.Artifact, error) {
	path := artifactPath(jobRef, attempt, name)
	if runID != "" {
		path += "?run_id=" + neturl.QueryEscape(runID)
	}
	var out threads.Artifact
	if err := c.do(ctx, http.MethodGet, path, token, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetArtifactBlobFromRun streams one file of an artifact, optionally from
// another run.
func (c *Client) GetArtifactBlobFromRun(ctx context.Context, token, jobRef string, attempt int, runID, name string, index int) (io.ReadCloser, int64, error) {
	path := artifactPath(jobRef, attempt, name, "blobs", strconv.Itoa(index))
	if runID != "" {
		path += "?run_id=" + neturl.QueryEscape(runID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Spindle-Runner", ProtocolVersion)
	hc := &http.Client{Timeout: 0}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, 0, apiErrorFrom(resp.StatusCode, body)
	}
	return resp.Body, resp.ContentLength, nil
}

// Package docker is a minimal Docker Engine API client over the local unix
// socket. Requests never go through HTTP proxies, mutations are never
// retried, and daemon error bodies (which can contain credentials, commands,
// and host paths) are discarded.
package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Tested Engine API range. Recursive read-only bind proofs need v1.44.
const (
	maxAPIMinor = 55
	minAPIMinor = 44
)

// APIError keeps only the HTTP status of a failed request.
type APIError struct{ Status int }

func (e *APIError) Error() string {
	return fmt.Sprintf("Docker API request failed (HTTP %d)", e.Status)
}

// StatusCode returns the HTTP status of a Docker API error, or 0.
func StatusCode(err error) int {
	var api *APIError
	if errors.As(err, &api) {
		return api.Status
	}
	return 0
}

// IsNotFound reports a 404 from Docker.
func IsNotFound(err error) bool { return StatusCode(err) == http.StatusNotFound }

// StreamOpenError reports a failed hijacked stream. A failed opening may
// already have dispatched a mutation; only RequestStarted == false proves
// that no request bytes reached the socket.
type StreamOpenError struct{ RequestStarted bool }

func (e *StreamOpenError) Error() string { return "Docker stream connection failed" }

// ErrReadTimeout reports a bounded read that took too long.
var ErrReadTimeout = errors.New("Docker read timed out")

var resourcePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// ContainerIDPattern matches Engine container references.
var ContainerIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

func resource(value string) (string, error) {
	if !resourcePattern.MatchString(value) {
		return "", errors.New("invalid Docker resource identifier")
	}
	return url.PathEscape(value), nil
}

// Client talks to one Docker daemon.
type Client struct {
	socket      string
	http        *http.Client
	readTimeout time.Duration

	mu      sync.Mutex
	version string
	// writes counts completed requests that may change Docker state.
	writes atomic.Uint64
}

// New returns a client for a unix socket path.
func New(socket string) *Client {
	transport := &http.Transport{
		// Daemon requests keep their own pool. Ambient proxy settings must
		// never redirect them.
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socket)
		},
		MaxIdleConns:    8,
		IdleConnTimeout: 30 * time.Second,
	}
	return &Client{socket: socket, readTimeout: 15 * time.Second, http: &http.Client{
		Transport: transport,
		// Never follow redirects from the daemon.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// Socket is the daemon socket path.
func (c *Client) Socket() string { return c.socket }

// Writes counts requests that may have changed Docker state. A cached read
// taken before the count last changed may be out of date.
func (c *Client) Writes() uint64 { return c.writes.Load() }

func apiMinor(value string) (int, error) {
	if !regexp.MustCompile(`^1\.\d+$`).MatchString(value) {
		return 0, errors.New("Docker returned an invalid API version")
	}
	return strconv.Atoi(value[2:])
}

func (c *Client) negotiate(ctx context.Context) (string, error) {
	c.mu.Lock()
	version := c.version
	c.mu.Unlock()
	if version != "" {
		return version, nil
	}
	var info struct{ ApiVersion, MinAPIVersion string }
	if err := c.read(ctx, "/version", func(body io.Reader) error { return json.NewDecoder(body).Decode(&info) }); err != nil {
		return "", err
	}
	minor, err := apiMinor(info.ApiVersion)
	if err != nil {
		return "", err
	}
	minor = min(minor, maxAPIMinor)
	if minor < minAPIMinor {
		return "", errors.New("Docker Engine has no supported API version for this client")
	}
	if info.MinAPIVersion != "" {
		least, err := apiMinor(info.MinAPIVersion)
		if err != nil || minor < least {
			return "", errors.New("Docker Engine has no supported API version for this client")
		}
	}
	version = fmt.Sprintf("v1.%d", minor)
	c.mu.Lock()
	c.version = version
	c.mu.Unlock()
	return version, nil
}

func (c *Client) path(ctx context.Context, path string, query url.Values) (string, error) {
	version, err := c.negotiate(ctx)
	if err != nil {
		return "", err
	}
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return "/" + version + path, nil
}

// Request sends a request and returns a successful response. The caller
// closes its body. Long streams have no deadline; their owners control it.
func (c *Client) Request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	return c.request(ctx, method, path, body, nil)
}

func (c *Client) request(ctx context.Context, method, path string, body any, header http.Header) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, reader)
	if err != nil {
		return nil, err
	}
	for name, values := range header {
		request.Header[name] = values
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if method != http.MethodGet && method != http.MethodHead {
		c.writes.Add(1)
	}
	if err != nil {
		return nil, errors.New("Docker request failed")
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
		response.Body.Close()
		return nil, &APIError{response.StatusCode}
	}
	return response, nil
}

// read bounds a complete GET including its body so stalled discovery can be
// retried.
func (c *Client) read(ctx context.Context, path string, decode func(io.Reader) error) error {
	ctx, cancel := context.WithTimeout(ctx, c.readTimeout)
	defer cancel()
	response, err := c.Request(ctx, http.MethodGet, path, nil)
	if err == nil {
		defer response.Body.Close()
		err = decode(response.Body)
		if err != nil && ctx.Err() == nil {
			var api *APIError
			if !errors.As(err, &api) {
				err = errors.New("Docker returned invalid JSON")
			}
		}
	}
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ErrReadTimeout
	}
	return err
}

func (c *Client) getJSON(ctx context.Context, path string, query url.Values, target any) error {
	full, err := c.path(ctx, path, query)
	if err != nil {
		return err
	}
	return c.read(ctx, full, func(body io.Reader) error { return json.NewDecoder(body).Decode(target) })
}

func (c *Client) postJSON(ctx context.Context, path string, query url.Values, body, target any) error {
	full, err := c.path(ctx, path, query)
	if err != nil {
		return err
	}
	response, err := c.Request(ctx, http.MethodPost, full, body)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if target == nil {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		return errors.New("Docker returned invalid JSON")
	}
	return nil
}

func (c *Client) empty(ctx context.Context, method, path string, query url.Values) error {
	full, err := c.path(ctx, path, query)
	if err != nil {
		return err
	}
	response, err := c.Request(ctx, method, full, nil)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
	return response.Body.Close()
}

func (c *Client) stream(ctx context.Context, path string, query url.Values) (io.ReadCloser, error) {
	full, err := c.path(ctx, path, query)
	if err != nil {
		return nil, err
	}
	response, err := c.Request(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, err
	}
	return response.Body, nil
}

// Ping checks the daemon and negotiates the API version.
func (c *Client) Ping(ctx context.Context) error {
	var text string
	err := c.read(ctx, "/_ping", func(body io.Reader) error {
		data, err := io.ReadAll(io.LimitReader(body, 1024))
		text = string(data)
		return err
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(text) != "OK" {
		return errors.New("invalid Docker ping response")
	}
	_, err = c.negotiate(ctx)
	return err
}

func filtersQuery(query url.Values, filters Filters) {
	if len(filters) > 0 {
		encoded, _ := json.Marshal(filters)
		query.Set("filters", string(encoded))
	}
}

// ListContainers lists containers, including stopped ones when all is set.
func (c *Client) ListContainers(ctx context.Context, all bool, filters Filters) ([]ContainerSummary, error) {
	query := url.Values{}
	if all {
		query.Set("all", "true")
	}
	filtersQuery(query, filters)
	// Decode before reading result: Go leaves the order of a plain operand
	// and a call in one return statement unspecified.
	var result []ContainerSummary
	err := c.getJSON(ctx, "/containers/json", query, &result)
	return result, err
}

func containerPath(id, suffix string) (string, error) {
	if !ContainerIDPattern.MatchString(id) {
		return "", errors.New("invalid Docker container identifier")
	}
	escaped, err := resource(id)
	return "/containers/" + escaped + suffix, err
}

// Inspect inspects a container.
func (c *Client) Inspect(ctx context.Context, id string) (*ContainerInspect, error) {
	path, err := containerPath(id, "/json")
	if err != nil {
		return nil, err
	}
	var info ContainerInspect
	return &info, c.getJSON(ctx, path, nil, &info)
}

// InspectSpec inspects a container keeping its configuration verbatim, for
// recreating it.
func (c *Client) InspectSpec(ctx context.Context, id string) (*ContainerSpec, error) {
	path, err := containerPath(id, "/json")
	if err != nil {
		return nil, err
	}
	var spec ContainerSpec
	return &spec, c.getJSON(ctx, path, nil, &spec)
}

// Rename renames a container.
func (c *Client) Rename(ctx context.Context, id, name string) error {
	path, err := containerPath(id, "/rename")
	if err != nil {
		return err
	}
	if !ContainerIDPattern.MatchString(name) {
		return errors.New("invalid Docker container name")
	}
	return c.empty(ctx, http.MethodPost, path, url.Values{"name": {name}})
}

// SetRestartPolicy replaces a container's restart policy, given as Docker
// reports it in HostConfig.RestartPolicy.
func (c *Client) SetRestartPolicy(ctx context.Context, id string, policy json.RawMessage) error {
	path, err := containerPath(id, "/update")
	if err != nil {
		return err
	}
	return c.postJSON(ctx, path, nil, map[string]json.RawMessage{"RestartPolicy": policy}, nil)
}

// Stats takes a one-shot stats sample.
func (c *Client) Stats(ctx context.Context, id string) (*ContainerStats, error) {
	path, err := containerPath(id, "/stats")
	if err != nil {
		return nil, err
	}
	var stats ContainerStats
	return &stats, c.getJSON(ctx, path, url.Values{"stream": {"false"}}, &stats)
}

// Start starts a container.
func (c *Client) Start(ctx context.Context, id string) error {
	path, err := containerPath(id, "/start")
	if err != nil {
		return err
	}
	return c.empty(ctx, http.MethodPost, path, nil)
}

func timeoutQuery(seconds *int) url.Values {
	if seconds == nil {
		return nil
	}
	return url.Values{"t": {strconv.Itoa(*seconds)}}
}

// Stop stops a container; seconds nil uses the daemon default.
func (c *Client) Stop(ctx context.Context, id string, seconds *int) error {
	path, err := containerPath(id, "/stop")
	if err != nil {
		return err
	}
	return c.empty(ctx, http.MethodPost, path, timeoutQuery(seconds))
}

// Restart restarts a container.
func (c *Client) Restart(ctx context.Context, id string, seconds *int) error {
	path, err := containerPath(id, "/restart")
	if err != nil {
		return err
	}
	return c.empty(ctx, http.MethodPost, path, timeoutQuery(seconds))
}

// Remove removes a container.
func (c *Client) Remove(ctx context.Context, id string, force, volumes bool) error {
	path, err := containerPath(id, "")
	if err != nil {
		return err
	}
	query := url.Values{}
	if force {
		query.Set("force", "true")
	}
	if volumes {
		query.Set("v", "true")
	}
	return c.empty(ctx, http.MethodDelete, path, query)
}

// Wait waits for a container to stop and returns its exit status.
func (c *Client) Wait(ctx context.Context, id string) (int, error) {
	path, err := containerPath(id, "/wait")
	if err != nil {
		return 0, err
	}
	var result struct{ StatusCode int }
	return result.StatusCode, c.postJSON(ctx, path, nil, nil, &result)
}

// LogsOptions selects container log output.
type LogsOptions struct {
	Follow, Stdout, Stderr, Timestamps bool
	Tail                               int // < 0 means all
}

// Logs streams container logs (multiplexed unless the container has a TTY).
func (c *Client) Logs(ctx context.Context, id string, options LogsOptions) (io.ReadCloser, error) {
	path, err := containerPath(id, "/logs")
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	for name, value := range map[string]bool{"follow": options.Follow, "stdout": options.Stdout,
		"stderr": options.Stderr, "timestamps": options.Timestamps} {
		if value {
			query.Set(name, "true")
		}
	}
	if options.Tail >= 0 {
		query.Set("tail", strconv.Itoa(options.Tail))
	}
	return c.stream(ctx, path, query)
}

// CreateContainer creates a container and returns its ID.
func (c *Client) CreateContainer(ctx context.Context, options ContainerCreate) (string, error) {
	return c.Create(ctx, options.Name, "", options)
}

// Create creates a container from any /containers/create body, such as one
// built from another container's inspection, and returns its ID. platform
// selects an image variant, or "" for the daemon's own.
func (c *Client) Create(ctx context.Context, name, platform string, body any) (string, error) {
	query := url.Values{}
	if name != "" {
		query.Set("name", name)
	}
	if platform != "" {
		query.Set("platform", platform)
	}
	var result struct{ Id string }
	if err := c.postJSON(ctx, "/containers/create", query, body, &result); err != nil {
		return "", err
	}
	if !ContainerIDPattern.MatchString(result.Id) {
		return "", errors.New("Docker returned an invalid container identifier")
	}
	return result.Id, nil
}

// CreateExec creates an exec instance.
func (c *Client) CreateExec(ctx context.Context, id string, options ExecCreate) (string, error) {
	path, err := containerPath(id, "/exec")
	if err != nil {
		return "", err
	}
	var result struct{ Id string }
	if err := c.postJSON(ctx, path, nil, options, &result); err != nil {
		return "", err
	}
	if !resourcePattern.MatchString(result.Id) {
		return "", errors.New("Docker returned an invalid exec identifier")
	}
	return result.Id, nil
}

// InspectExec reads an exec's status.
func (c *Client) InspectExec(ctx context.Context, execID string) (*ExecInspect, error) {
	escaped, err := resource(execID)
	if err != nil {
		return nil, err
	}
	var info ExecInspect
	return &info, c.getJSON(ctx, "/exec/"+escaped+"/json", nil, &info)
}

// StartExec starts an exec and hijacks its stream.
func (c *Client) StartExec(ctx context.Context, execID string) (*Conn, error) {
	escaped, err := resource(execID)
	if err != nil {
		return nil, err
	}
	path, err := c.path(ctx, "/exec/"+escaped+"/start", nil)
	if err != nil {
		return nil, err
	}
	return c.hijack(ctx, path, map[string]bool{"Detach": false, "Tty": false})
}

// AttachOptions selects attached streams.
type AttachOptions struct{ Stream, Stdin, Stdout, Stderr bool }

// Attach hijacks a container's stdio.
func (c *Client) Attach(ctx context.Context, id string, options AttachOptions) (*Conn, error) {
	path, err := containerPath(id, "/attach")
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	for name, value := range map[string]bool{"stream": options.Stream, "stdin": options.Stdin,
		"stdout": options.Stdout, "stderr": options.Stderr} {
		if value {
			query.Set(name, "true")
		}
	}
	full, err := c.path(ctx, path, query)
	if err != nil {
		return nil, err
	}
	return c.hijack(ctx, full, nil)
}

// InspectVolume inspects a named volume.
func (c *Client) InspectVolume(ctx context.Context, name string) (*VolumeInspect, error) {
	escaped, err := resource(name)
	if err != nil {
		return nil, err
	}
	var info VolumeInspect
	return &info, c.getJSON(ctx, "/volumes/"+escaped, nil, &info)
}

// CreateVolume creates a named volume.
func (c *Client) CreateVolume(ctx context.Context, name string) (*VolumeInspect, error) {
	var info VolumeInspect
	return &info, c.postJSON(ctx, "/volumes/create", nil, map[string]string{"Name": name}, &info)
}

// RemoveVolume removes a named volume.
func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	escaped, err := resource(name)
	if err != nil {
		return err
	}
	return c.empty(ctx, http.MethodDelete, "/volumes/"+escaped, nil)
}

// InspectImage returns an image's ID. The whole reference is one escaped
// path segment so registry ports, slashes, and digests cannot become query
// parameters or another endpoint.
func (c *Client) InspectImage(ctx context.Context, reference string) (string, error) {
	if reference == "" || reference == "." || reference == ".." {
		return "", errors.New("invalid Docker image reference")
	}
	info, err := c.InspectImageConfig(ctx, reference)
	if err != nil {
		return "", err
	}
	return info.ID, nil
}

// InspectImageConfig returns an image's ID, the defaults it gives
// containers, and its platform manifests. The reference is escaped as
// InspectImage's is.
func (c *Client) InspectImageConfig(ctx context.Context, reference string) (*ImageInspect, error) {
	return c.inspectImage(ctx, reference, url.Values{"manifests": {"1"}})
}

// InspectImageVariant returns the defaults one platform variant of an image
// gives containers. With several variants stored, Docker otherwise answers
// for its own platform. Its ID is the variant's, not the image's.
func (c *Client) InspectImageVariant(ctx context.Context, reference string, platform Platform) (*ImageInspect, error) {
	encoded, _ := json.Marshal(platform)
	return c.inspectImage(ctx, reference, url.Values{"platform": {string(encoded)}})
}

func (c *Client) inspectImage(ctx context.Context, reference string, query url.Values) (*ImageInspect, error) {
	if reference == "" || reference == "." || reference == ".." {
		return nil, errors.New("invalid Docker image reference")
	}
	var info ImageInspect
	return &info, c.getJSON(ctx, "/images/"+url.PathEscape(reference)+"/json", query, &info)
}

// Events streams container events.
func (c *Client) Events(ctx context.Context, filters Filters) (io.ReadCloser, error) {
	query := url.Values{}
	filtersQuery(query, filters)
	return c.stream(ctx, "/events", query)
}

// Pull pulls an image and checks every progress record. HTTP 200 alone does
// not mean a pull succeeded; Docker reports registry errors in the body.
// registryAuth is an X-Registry-Auth value, or "" for anonymous access;
// platform selects an image variant, or "" for the daemon's own.
func (c *Client) Pull(ctx context.Context, image, registryAuth, platform string) error {
	query := url.Values{"fromImage": {image}}
	if platform != "" {
		query.Set("platform", platform)
	}
	full, err := c.path(ctx, "/images/create", query)
	if err != nil {
		return err
	}
	var header http.Header
	if registryAuth != "" {
		header = http.Header{"X-Registry-Auth": {registryAuth}}
	}
	response, err := c.request(ctx, http.MethodPost, full, nil, header)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	records := 0
	var failure error
	decodeErr := DecodeJSONLines(response.Body, 1<<20, false, func(raw json.RawMessage) error {
		var record map[string]json.RawMessage
		if json.Unmarshal(raw, &record) != nil || record == nil {
			return errors.New("Docker returned invalid pull progress")
		}
		_, hasError := record["error"]
		_, hasDetail := record["errorDetail"]
		if hasError || hasDetail {
			failure = &APIError{500}
			return failure
		}
		records++
		return nil
	})
	if failure != nil {
		return failure
	}
	if decodeErr != nil || records == 0 {
		return errors.New("Docker returned invalid or oversized pull progress")
	}
	return nil
}

// Conn is a hijacked Docker stream. Closing the write side sends stdin EOF
// while output keeps flowing.
type Conn struct {
	conn   net.Conn
	reader *bufio.Reader
}

// Read reads raw (possibly multiplexed) output.
func (c *Conn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// Write writes stdin.
func (c *Conn) Write(p []byte) (int, error) { return c.conn.Write(p) }

// CloseWrite sends stdin EOF.
func (c *Conn) CloseWrite() error {
	if closer, ok := c.conn.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return nil
}

// Close terminates both directions.
func (c *Conn) Close() error { return c.conn.Close() }

var rawStreamType = regexp.MustCompile(`(?i)^application/vnd\.docker\.(?:raw|multiplexed)-stream(?:;|$)`)

type countingConn struct {
	net.Conn
	started bool
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.started = true
	}
	return n, err
}

func (c *Client) hijack(ctx context.Context, path string, body any) (*Conn, error) {
	payload := []byte{}
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	var dialer net.Dialer
	raw, err := dialer.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return nil, &StreamOpenError{RequestStarted: false}
	}
	conn := &countingConn{Conn: raw}
	fail := func(err error) (*Conn, error) {
		raw.Close()
		var api *APIError
		if errors.As(err, &api) {
			return nil, err
		}
		return nil, &StreamOpenError{RequestStarted: conn.started}
	}
	// Docker can accept a request and never answer it. The caller's deadline
	// must still end the opening and close the socket.
	stop := context.AfterFunc(ctx, func() { raw.SetDeadline(time.Unix(1, 0)) })
	request := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n"+
		"Content-Type: application/json\r\nContent-Length: %d\r\n\r\n", path, len(payload))
	if _, err := conn.Write(append([]byte(request), payload...)); err != nil {
		stop()
		return fail(err)
	}
	limited := &headerLimit{conn: raw, limit: 16384}
	reader := bufio.NewReaderSize(limited, 16384)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		stop()
		return fail(err)
	}
	limited.done = true
	if response.StatusCode != http.StatusSwitchingProtocols && response.StatusCode != http.StatusOK {
		stop()
		return fail(&APIError{response.StatusCode})
	}
	header := response.Header
	upgrade := false
	for _, value := range strings.Split(header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(value), "upgrade") {
			upgrade = true
		}
	}
	// Docker also documents a legacy 200 raw-stream response. It is already
	// hijacked; HTTP chunk framing must never reach stdin or stdout.
	if len(response.TransferEncoding) > 0 || header.Get("Transfer-Encoding") != "" ||
		(response.StatusCode == http.StatusSwitchingProtocols && (!strings.EqualFold(header.Get("Upgrade"), "tcp") || !upgrade)) ||
		(response.StatusCode == http.StatusOK && !rawStreamType.MatchString(header.Get("Content-Type"))) {
		stop()
		return fail(errors.New("invalid Docker stream upgrade"))
	}
	if !stop() {
		return fail(ctx.Err())
	}
	raw.SetDeadline(time.Time{})
	return &Conn{conn: raw, reader: reader}, nil
}

// headerLimit bounds the upgrade headers to 16 KiB. After the headers the
// stream is unlimited.
type headerLimit struct {
	conn  net.Conn
	limit int
	read  int
	done  bool
}

func (r *headerLimit) Read(p []byte) (int, error) {
	if !r.done && r.read >= r.limit {
		return 0, errors.New("Docker upgrade headers exceeded their limit")
	}
	n, err := r.conn.Read(p)
	r.read += n
	return n, err
}

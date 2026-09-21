package provider

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// The bounds on a turn. None is a wall-clock cap on the exchange, on purpose, since http.Client.Timeout would kill a
// stream that is still producing tokens. What is bounded is silence - waiting for a connection, for the response head,
// and between two reads of the body (see stallReader). A turn that is still producing is working, not hung.
const (
	dialTimeout           = 30 * time.Second
	responseHeaderTimeout = 10 * time.Minute
)

// streamStallTimeout is how long a stream may say nothing at all before it is
// treated as hung. A variable only so tests can drive it without waiting
// minutes. It is not a configuration knob.
var streamStallTimeout = 10 * time.Minute

// newHTTPClient builds the shared transport, bounding the phases that can hang
// without bounding the one that takes a long time.
func newHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()

	transport.DialContext = (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = dialTimeout
	transport.ResponseHeaderTimeout = responseHeaderTimeout

	return &http.Client{Transport: stallTransport{base: transport}}
}

// stallTransport puts a stallReader on every response body, so a stream that
// goes silent fails - and an error body that is held open does too.
type stallTransport struct {
	base http.RoundTripper
}

// stallReader fails a stream that has gone silent, without bounding one that is still producing. The deadline moves
// on every read that returns data. Firing closes the body, and the error is replaced with one naming the stall,
// since "use of closed network connection" is neither true nor recognizable as transient.
type stallReader struct {
	inner   io.ReadCloser
	timeout time.Duration
	timer   *time.Timer

	mu      sync.Mutex
	stalled bool
}

func (r *stallReader) fire() {
	r.mu.Lock()

	r.stalled = true

	r.mu.Unlock()

	_ = r.inner.Close()
}

func newStallReader(inner io.ReadCloser, timeout time.Duration) *stallReader {
	reader := &stallReader{inner: inner, timeout: timeout}

	reader.timer = time.AfterFunc(timeout, reader.fire)

	return reader
}

func (t stallTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}

	response.Body = newStallReader(response.Body, streamStallTimeout)

	return response, nil
}

// errStreamStalled is what a stream that went silent fails with. A sentinel, so
// the retry rules can recognize it by type rather than by its wording.
var errStreamStalled = errors.New("the stream stalled")

func (r *stallReader) didStall() bool {
	r.mu.Lock()

	defer r.mu.Unlock()

	return r.stalled
}

func (r *stallReader) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)

	if n > 0 {
		r.timer.Reset(r.timeout)
	}

	if err != nil && r.didStall() {
		return n, fmt.Errorf("%w: nothing arrived for %s", errStreamStalled, r.timeout)
	}

	return n, err
}

// Close releases the watchdog along with the body.
func (r *stallReader) Close() error {
	r.timer.Stop()

	return r.inner.Close()
}

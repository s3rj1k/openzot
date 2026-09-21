// Package testutils holds the helpers the tests of several packages share. Only test packages import it.
package testutils

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/session"
)

// Write writes content to path, creating the directories above it.
func Write(t *testing.T, path, content string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755)) //nolint:gosec // G301: a test fixture directory

	require.NoError(t, os.WriteFile(path, []byte(content), 0o644)) //nolint:gosec // G306: a test fixture file
}

// WriteConfig writes body as a config file in a fresh temporary directory and returns its path.
func WriteConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	return path
}

// ReadLog reads a session log the way anyone does. One JSON value per line, and a line that is not JSON fails the
// test, since the format is the contract.
func ReadLog(t *testing.T, path string) []session.Record {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // G304: the test names the log
	require.NoError(t, err)

	if len(data) > 0 {
		require.Equal(t, byte('\n'), data[len(data)-1], "log does not end on a line")
	}

	var records []session.Record

	for i, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}

		var record session.Record

		err := json.Unmarshal([]byte(line), &record)
		require.NoError(t, err, "line %d is not a JSON record: %v\n%s", i+1, err, line)

		records = append(records, record)
	}

	return records
}

// Capture redirects one of the process's standard streams for the duration of a call and returns what was written to it.
func Capture(t *testing.T, stream **os.File, fn func() error) (string, error) {
	t.Helper()

	original := *stream

	read, write, err := os.Pipe()
	require.NoError(t, err)

	*stream = write

	done := make(chan string)

	go func() {
		var builder strings.Builder

		buffer := make([]byte, 4096)

		for {
			n, err := read.Read(buffer)

			builder.Write(buffer[:n])

			if err != nil {
				break
			}
		}

		done <- builder.String()
	}()

	runErr := fn()

	*stream = original

	_ = write.Close()

	return <-done, runErr
}

// CaptureStdout collects what a function prints to stdout.
func CaptureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	return Capture(t, &os.Stdout, fn)
}

// CaptureStderr collects what a function prints to stderr.
func CaptureStderr(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	return Capture(t, &os.Stderr, fn)
}

// SilenceStderr discards stderr for the rest of a test that by design triggers the usage block.
func SilenceStderr(t *testing.T) {
	t.Helper()

	original := os.Stderr

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	require.NoError(t, err)

	os.Stderr = devNull

	t.Cleanup(func() {
		os.Stderr = original

		devNull.Close()
	})
}

// Kinds is the kind of each record of a session log, in order.
func Kinds(records []session.Record) []session.Kind {
	out := make([]session.Kind, 0, len(records))

	for _, record := range records {
		out = append(out, record.Kind)
	}

	return out
}

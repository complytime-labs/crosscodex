// Command traceload reads OTLP JSON trace files and POSTs them to an
// OTLP HTTP endpoint. Each non-empty line in a .jsonl file is one
// ExportTraceServiceRequest, sent as-is.
//
// Usage:
//
//	go run ./test/traceload [--dir <path>] [--endpoint <url>]
//
// Defaults:
//
//	--dir       .test-output/traces
//	--endpoint  http://localhost:14318/v1/traces
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	dir := flag.String("dir", ".test-output/traces", "directory containing OTLP JSONL trace files")
	endpoint := flag.String("endpoint", "http://localhost:14318/v1/traces", "OTLP HTTP endpoint")
	flag.Parse()

	if err := run(*dir, *endpoint); err != nil {
		fmt.Fprintf(os.Stderr, "traceload: %v\n", err)
		os.Exit(1)
	}
}

func run(dir, endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("invalid endpoint URL %q — expected http(s)://host:port/path", endpoint)
	}

	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return fmt.Errorf("glob %q: %w", dir, err)
	}
	if len(files) == 0 {
		return fmt.Errorf("no .jsonl files found in %q — run tests with TEST_TRACE_DIR set first", dir)
	}

	client := &http.Client{}
	var totalLines, totalFiles int

	for _, path := range files {
		n, err := postFile(client, path, endpoint)
		if err != nil {
			return err
		}
		totalLines += n
		totalFiles++
	}

	fmt.Printf("traceload: sent %d trace batches from %d files to %s\n", totalLines, totalFiles, endpoint)
	return nil
}

func postFile(client *http.Client, path, endpoint string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Allow up to 10 MB per line for large trace batches.
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var count int
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(line))
		if err != nil {
			return count, fmt.Errorf("create request for %q line %d: %w", path, count+1, err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return count, fmt.Errorf("POST %q line %d: %w", path, count+1, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return count, fmt.Errorf("POST %q line %d: HTTP %d: %s",
				path, count+1, resp.StatusCode, strings.TrimSpace(string(body)))
		}
		count++
	}

	if err := scanner.Err(); err != nil {
		return count, fmt.Errorf("read %q: %w", path, err)
	}
	return count, nil
}

package main

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

// SchedulerClient proxies HTTP requests to relayScheduler.
type SchedulerClient struct {
	baseURL string
	http    *http.Client
}

func NewSchedulerClient(baseURL string) *SchedulerClient {
	return &SchedulerClient{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Proxy forwards an HTTP request to the scheduler and returns status, body, error.
func (c *SchedulerClient) Proxy(method, path string, query string, body io.Reader) (int, []byte, error) {
	url := c.baseURL + path
	if query != "" {
		url += "?" + query
	}

	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return 0, nil, fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("scheduler request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read response: %w", err)
	}

	return resp.StatusCode, data, nil
}

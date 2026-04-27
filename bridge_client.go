package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// bridgeClient connects to relay's bridge socket to query project data.
// Implements BridgeProjectClient for use in API proxy handlers.
type bridgeClient struct {
	sockPath string
	token    string
}

func newBridgeClient(token string) *bridgeClient {
	configDir, err := os.UserConfigDir()
	if err != nil {
		configDir, _ = os.UserHomeDir()
	}
	return &bridgeClient{
		sockPath: filepath.Join(configDir, "relay", "relay.sock"),
		token:    token,
	}
}

type bridgeRequest struct {
	Type      string `json:"type"`
	Token     string `json:"token,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
}

type bridgeResponse struct {
	Type    string          `json:"type"`
	Data    json.RawMessage `json:"data,omitempty"`
	Code    int             `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
}

func (c *bridgeClient) ListProjects() (json.RawMessage, error) {
	resp, err := c.send(bridgeRequest{Type: "ListProjects", Token: c.token})
	if err != nil {
		return nil, err
	}
	if resp.Type == "Error" {
		return nil, fmt.Errorf("bridge error (code %d): %s", resp.Code, resp.Message)
	}
	return resp.Data, nil
}

func (c *bridgeClient) GetProject(id string) (json.RawMessage, error) {
	resp, err := c.send(bridgeRequest{Type: "GetProject", Token: c.token, ProjectID: id})
	if err != nil {
		return nil, err
	}
	if resp.Type == "Error" {
		return nil, fmt.Errorf("bridge error (code %d): %s", resp.Code, resp.Message)
	}
	return resp.Data, nil
}

func (c *bridgeClient) send(req bridgeRequest) (*bridgeResponse, error) {
	conn, err := net.Dial("unix", c.sockPath)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to relay bridge: %w", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(10 * time.Second))

	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		return nil, fmt.Errorf("bridge write failed: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("bridge read failed: %w", err)
		}
		return nil, fmt.Errorf("bridge closed connection")
	}

	var resp bridgeResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("bridge parse failed: %w", err)
	}
	return &resp, nil
}

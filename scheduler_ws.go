package main

import (
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// SchedulerWSForwarder connects to relayScheduler's WebSocket and broadcasts
// task lifecycle events to all connected Eve clients via WSHub.
type SchedulerWSForwarder struct {
	schedulerURL string
	hub          *WSHub
	done         chan struct{}
	closeOnce    sync.Once
}

func NewSchedulerWSForwarder(schedulerHTTPURL string, hub *WSHub) *SchedulerWSForwarder {
	wsURL := strings.Replace(schedulerHTTPURL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
	wsURL += "/ws"

	return &SchedulerWSForwarder{
		schedulerURL: wsURL,
		hub:          hub,
		done:         make(chan struct{}),
	}
}

// Run connects to the scheduler WebSocket and forwards events. Blocks until Close is called.
func (f *SchedulerWSForwarder) Run() {
	for {
		select {
		case <-f.done:
			return
		default:
		}

		f.connectAndForward()

		// Wait before reconnecting
		select {
		case <-f.done:
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (f *SchedulerWSForwarder) connectAndForward() {
	conn, _, err := websocket.DefaultDialer.Dial(f.schedulerURL, nil)
	if err != nil {
		slog.Debug("scheduler WS connect failed", "url", f.schedulerURL, "error", err)
		return
	}
	defer conn.Close()

	slog.Info("connected to scheduler WebSocket", "url", f.schedulerURL)

	for {
		select {
		case <-f.done:
			return
		default:
		}

		_, msgBytes, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				slog.Error("scheduler WS read error", "error", err)
			}
			return
		}

		var msg map[string]interface{}
		if err := json.Unmarshal(msgBytes, &msg); err != nil {
			continue
		}

		f.hub.Broadcast(msg)
	}
}

// Close stops the forwarder.
func (f *SchedulerWSForwarder) Close() {
	f.closeOnce.Do(func() {
		close(f.done)
	})
}

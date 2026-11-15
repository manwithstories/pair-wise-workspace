package signal

import (
	"context"
	"testing"
	"time"
)

func TestServerReclaimsClientWithoutHeartbeat(t *testing.T) {
	server, err := NewServer(ServerConfig{HeartbeatTimeout: 100 * time.Millisecond, HandshakeTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client := NewClient(ClientConfig{
		URL: server.URL(), Stream: "heartbeat", Role: "quiet",
		HeartbeatInterval: time.Hour, HandshakeTimeout: time.Second, Reconnect: false,
	})
	client.Start()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if client.Connected() {
		t.Fatal("quiet client should have been disconnected")
	}
	_ = client.Close()
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.streams) != 0 {
		t.Fatalf("streams were not reclaimed: %d", len(server.streams))
	}
}

func TestHeartbeatKeepsClientRegistered(t *testing.T) {
	server, err := NewServer(ServerConfig{HeartbeatTimeout: 100 * time.Millisecond, HandshakeTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client := NewClient(ClientConfig{
		URL: server.URL(), Stream: "heartbeat", Role: "active",
		HeartbeatInterval: 20 * time.Millisecond, HandshakeTimeout: time.Second, Reconnect: false,
	})
	client.Start()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if !client.Connected() {
		t.Fatal("heartbeat client was disconnected")
	}
	_ = client.Close()
}

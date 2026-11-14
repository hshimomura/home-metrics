package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

func mqttTestBroker(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		serve(conn)
	}()
	return listener.Addr().String()
}
func brokerHandshake(conn net.Conn) error {
	if _, _, err := readMQTTPacket(conn, 1024); err != nil {
		return err
	}
	if err := writeMQTTPacket(conn, 0x20, []byte{0, 0}); err != nil {
		return err
	}
	if _, _, err := readMQTTPacket(conn, 1024); err != nil {
		return err
	}
	return writeMQTTPacket(conn, 0x90, []byte{0, 1, 0})
}
func TestMQTTHandshakeCancellation(t *testing.T) {
	addr := mqttTestBroker(t, func(conn net.Conn) { io.Copy(io.Discard, conn) })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := runMQTT(ctx, config{MQTTAddr: addr}, nil, nil, nil); err == nil {
		t.Fatal("expected cancellation")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("handshake ignored cancellation")
	}
}
func TestMQTTIdleTimeoutPreservesNextPacket(t *testing.T) {
	addr := mqttTestBroker(t, func(conn net.Conn) {
		if brokerHandshake(conn) != nil {
			return
		}
		time.Sleep(1200 * time.Millisecond)
		writeMQTTPacket(conn, 0x30, []byte{0, 1, 'x', 'o', 'k'})
		io.Copy(io.Discard, conn)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	calls := 0
	err := runMQTT(ctx, config{MQTTAddr: addr, Topic: "x", StreamHeartbeat: time.Minute}, nil, func(topic string, payload []byte) error {
		calls++
		if string(payload) != "ok" {
			t.Errorf("payload=%q", payload)
		}
		cancel()
		return nil
	}, nil)
	if calls != 1 || err == nil {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
func TestMQTTPartialFrameTimeoutClosesStream(t *testing.T) {
	for _, partial := range [][]byte{{0x30}, {0x30, 0x80}, {0x30, 5, 0, 1}} {
		t.Run(fmt.Sprintf("%x", partial), func(t *testing.T) {
			t.Parallel()
			addr := mqttTestBroker(t, func(conn net.Conn) {
				if brokerHandshake(conn) != nil {
					return
				}
				conn.Write(partial)
				io.Copy(io.Discard, conn)
			})
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			calls := 0
			err := runMQTT(ctx, config{MQTTAddr: addr, StreamHeartbeat: time.Minute}, nil, func(string, []byte) error { calls++; return nil }, nil)
			var ne net.Error
			if !errors.As(err, &ne) || !ne.Timeout() || ctx.Err() != nil || calls != 0 {
				t.Fatalf("partial packet retried or emitted calls=%d err=%v", calls, err)
			}
		})
	}
}
func TestMQTTConsumerFailureEndsStream(t *testing.T) {
	addr := mqttTestBroker(t, func(conn net.Conn) {
		if brokerHandshake(conn) != nil {
			return
		}
		writeMQTTPacket(conn, 0x30, []byte{0, 1, 'x', 'y'})
		io.Copy(io.Discard, conn)
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	failure := errors.New("queue full")
	err := runMQTT(ctx, config{MQTTAddr: addr, Topic: "x", StreamHeartbeat: time.Minute}, nil, func(string, []byte) error { return failure }, nil)
	if !errors.Is(err, failure) {
		t.Fatalf("consumer error hidden: %v", err)
	}
}

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

func runMQTT(ctx context.Context, cfg config, onReady func(), onMessage func(topic string, payload []byte), onFlushTick func()) error {
	conn, err := net.DialTimeout("tcp", cfg.MQTTAddr, 10*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	clientID := "home-metrics-" + randomHex(4)
	if err := mqttConnect(conn, clientID, cfg.DataAppID, cfg.DataAPIKey, cfg.MQTTMaxPacket); err != nil {
		return err
	}
	if err := mqttSubscribe(conn, 1, cfg.Topic, cfg.MQTTMaxPacket); err != nil {
		return err
	}
	if onReady != nil {
		onReady()
	}
	heartbeat := time.NewTicker(cfg.StreamHeartbeat)
	defer heartbeat.Stop()
	var flushTick <-chan time.Time
	var flushTicker *time.Ticker
	if cfg.AggregateFlush > 0 {
		flushTicker = time.NewTicker(cfg.AggregateFlush)
		flushTick = flushTicker.C
		defer flushTicker.Stop()
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-heartbeat.C:
			if err := writeMQTTPacket(conn, 0xc0, nil); err != nil {
				return err
			}
		case <-flushTick:
			if onFlushTick != nil {
				onFlushTick()
			}
		default:
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		packetType, payload, err := readMQTTPacket(conn, cfg.MQTTMaxPacket)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return err
		}
		if packetType == 3 {
			topic, body, err := parsePublish(payload)
			if err != nil {
				return err
			}
			onMessage(topic, body)
		}
	}
}

func mqttConnect(conn net.Conn, clientID string, username string, password string, maxPacket int) error {
	var variable bytes.Buffer
	writeMQTTString(&variable, "MQTT")
	variable.WriteByte(4)
	variable.WriteByte(0x80 | 0x40 | 0x02)
	_ = binary.Write(&variable, binary.BigEndian, uint16(60))
	writeMQTTString(&variable, clientID)
	writeMQTTString(&variable, username)
	writeMQTTString(&variable, password)
	if err := writeMQTTPacket(conn, 0x10, variable.Bytes()); err != nil {
		return err
	}
	packetType, payload, err := readMQTTPacket(conn, maxPacket)
	if err != nil {
		return err
	}
	if packetType != 2 || len(payload) < 2 {
		return fmt.Errorf("unexpected MQTT CONNACK packet type=%d", packetType)
	}
	if payload[1] != 0 {
		return fmt.Errorf("MQTT connect rejected code=%d", payload[1])
	}
	return nil
}

func mqttSubscribe(conn net.Conn, packetID uint16, topic string, maxPacket int) error {
	var payload bytes.Buffer
	_ = binary.Write(&payload, binary.BigEndian, packetID)
	writeMQTTString(&payload, topic)
	payload.WriteByte(0)
	if err := writeMQTTPacket(conn, 0x82, payload.Bytes()); err != nil {
		return err
	}
	packetType, body, err := readMQTTPacket(conn, maxPacket)
	if err != nil {
		return err
	}
	if packetType != 9 || len(body) < 3 {
		return fmt.Errorf("unexpected MQTT SUBACK packet type=%d", packetType)
	}
	if body[len(body)-1] == 0x80 {
		return errors.New("MQTT subscribe rejected")
	}
	return nil
}

func parsePublish(payload []byte) (string, []byte, error) {
	if len(payload) < 2 {
		return "", nil, io.ErrUnexpectedEOF
	}
	topicLen := int(binary.BigEndian.Uint16(payload[:2]))
	if len(payload) < 2+topicLen {
		return "", nil, io.ErrUnexpectedEOF
	}
	return string(payload[2 : 2+topicLen]), payload[2+topicLen:], nil
}

func writeMQTTPacket(conn net.Conn, header byte, payload []byte) error {
	var frame bytes.Buffer
	frame.WriteByte(header)
	writeRemainingLength(&frame, len(payload))
	frame.Write(payload)
	_, err := conn.Write(frame.Bytes())
	return err
}

func readMQTTPacket(conn io.Reader, maxPacket int) (int, []byte, error) {
	header := make([]byte, 1)
	if _, err := io.ReadFull(conn, header); err != nil {
		return 0, nil, err
	}
	length, err := readRemainingLength(conn)
	if err != nil {
		return 0, nil, err
	}
	if maxPacket <= 0 {
		maxPacket = defaultMQTTMaxPacket
	}
	if length > maxPacket {
		return 0, nil, fmt.Errorf("MQTT packet too large: %d bytes exceeds %d", length, maxPacket)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return 0, nil, err
	}
	return int(header[0] >> 4), payload, nil
}

func writeRemainingLength(buf *bytes.Buffer, length int) {
	for {
		encoded := byte(length % 128)
		length /= 128
		if length > 0 {
			encoded |= 128
		}
		buf.WriteByte(encoded)
		if length == 0 {
			return
		}
	}
}

func readRemainingLength(r io.Reader) (int, error) {
	var multiplier int = 1
	var value int
	for i := 0; i < 4; i++ {
		var encoded [1]byte
		if _, err := io.ReadFull(r, encoded[:]); err != nil {
			return 0, err
		}
		value += int(encoded[0]&127) * multiplier
		if encoded[0]&128 == 0 {
			return value, nil
		}
		multiplier *= 128
	}
	return 0, errors.New("malformed MQTT remaining length")
}

func writeMQTTString(buf *bytes.Buffer, value string) {
	_ = binary.Write(buf, binary.BigEndian, uint16(len(value)))
	buf.WriteString(value)
}

func randomHex(bytesLen int) string {
	buf := make([]byte, bytesLen)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buf)
}

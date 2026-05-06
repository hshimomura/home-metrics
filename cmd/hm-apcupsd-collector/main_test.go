package main

import (
	"encoding/binary"
	"net"
	"testing"
	"time"
)

const sampleStatus = `APC      : 001,036,0859
DATE     : 2026-05-05 21:17:45 +0900  
HOSTNAME : ups-host
MODEL    : APC RS 550S  
STATUS   : ONLINE 
LINEV    : 102.0 Volts
LOADPCT  : 45.0 Percent
BCHARGE  : 100.0 Percent
BATTV    : 13.6 Volts
END APC  : 2026-05-05 21:17:58 +0900  
`

func TestParseStatus(t *testing.T) {
	fallback := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	status, err := parseStatus(sampleStatus, fallback)
	if err != nil {
		t.Fatalf("parseStatus returned error: %v", err)
	}
	if status.TS.Format(time.RFC3339) != "2026-05-05T21:17:45+09:00" {
		t.Fatalf("TS = %s", status.TS.Format(time.RFC3339))
	}
	if status.InputVoltage != 102.0 || status.LoadPercent != 45.0 || status.BatteryCharge != 100.0 || status.BatteryVoltage != 13.6 {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestParseServer(t *testing.T) {
	network, address, err := parseServer("tcp://127.0.0.1:3551")
	if err != nil {
		t.Fatalf("parseServer returned error: %v", err)
	}
	if network != "tcp" || address != "127.0.0.1:3551" {
		t.Fatalf("parseServer = %s %s", network, address)
	}
}

func TestReadNISResponseAcceptsEOFWithoutTerminator(t *testing.T) {
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() {
		defer server.Close()
		payload := []byte("APC      : 001,036,0859\n")
		var header [2]byte
		binary.BigEndian.PutUint16(header[:], uint16(len(payload)))
		if _, err := server.Write(header[:]); err != nil {
			done <- err
			return
		}
		if _, err := server.Write(payload); err != nil {
			done <- err
			return
		}
		done <- nil
	}()

	got, err := readNISResponse(client)
	if err != nil {
		t.Fatalf("readNISResponse returned error: %v", err)
	}
	if got != "APC      : 001,036,0859\n" {
		t.Fatalf("readNISResponse = %q", got)
	}
	if err := <-done; err != nil {
		t.Fatalf("server write: %v", err)
	}
}

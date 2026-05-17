package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

func streamWithReconnect(ctx context.Context, client *http.Client, cfg config, handleError func(error), handleHeartbeat func(), handle func(firehoseEvent, []byte)) {
	backoff := cfg.ReconnectMinDelay
	for ctx.Err() == nil {
		err := streamOnce(ctx, client, cfg, handleHeartbeat, handle)
		if ctx.Err() != nil {
			return
		}
		log.Printf("Cisco Spaces stream ended: %v; reconnecting in %s", err, backoff)
		if handleError != nil {
			handleError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > cfg.ReconnectMaxDelay {
			backoff = cfg.ReconnectMaxDelay
		}
	}
}

func streamOnce(ctx context.Context, client *http.Client, cfg config, handleHeartbeat func(), handle func(firehoseEvent, []byte)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.FirehoseURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", cfg.APIKey)

	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
		return fmt.Errorf("firehose status=%d body=%s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	if handleHeartbeat != nil {
		heartbeatCtx, cancelHeartbeat := context.WithCancel(ctx)
		defer cancelHeartbeat()
		handleHeartbeat()
		go streamHeartbeat(heartbeatCtx, cfg.StreamHeartbeat, handleHeartbeat)
	}

	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event firehoseEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			log.Printf("decode Cisco Spaces event: %v", err)
			continue
		}
		handle(event, []byte(line))
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return errors.New("firehose response closed")
}

func streamHeartbeat(ctx context.Context, interval time.Duration, handleHeartbeat func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			handleHeartbeat()
		}
	}
}

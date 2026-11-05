// Package notify delivers alerts. Prototype: Slack incoming webhook only.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Slack posts a plain text message to an incoming webhook URL.
func Slack(ctx context.Context, webhook, text string) error {
	if webhook == "" {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("slack webhook: HTTP %d", res.StatusCode)
	}
	return nil
}

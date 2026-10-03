// Package notify pushes "this needs you" events out of the board.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Event struct {
	Repo   string
	Number int
	Title  string
	URL    string
	Reason string
}

type Notifier interface {
	Notify(ctx context.Context, e Event) error
}

func (e Event) text() string {
	return fmt.Sprintf("%s#%d needs you: %s\n%s", e.Repo, e.Number, e.Reason, e.Title)
}

// Multi fans an event out to every notifier and reports all failures.
type Multi []Notifier

func (m Multi) Notify(ctx context.Context, e Event) error {
	var errs []error
	for _, n := range m {
		errs = append(errs, n.Notify(ctx, e))
	}
	return errors.Join(errs...)
}

func postJSON(ctx context.Context, url string, payload any) (int, []byte, error) {
	b, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, body, nil
}

// Telegram sends through a bot (token from @BotFather) to one chat.
type Telegram struct{ Token, ChatID string }

func (t Telegram) Notify(ctx context.Context, e Event) error {
	if t.Token == "" || t.ChatID == "" {
		return nil
	}
	status, body, err := postJSON(ctx, "https://api.telegram.org/bot"+t.Token+"/sendMessage", map[string]any{
		"chat_id":                  t.ChatID,
		"text":                     e.text() + "\n" + e.URL,
		"disable_web_page_preview": true,
	})
	if err != nil {
		// The URL holds the bot token; keep it out of logs and the UI.
		return errors.New("telegram: request failed")
	}
	if status >= 300 {
		var r struct{ Description string }
		_ = json.Unmarshal(body, &r)
		return fmt.Errorf("telegram: %d %s", status, r.Description)
	}
	return nil
}

// Webhook posts a {"text": ...} payload, which is what Slack, Mattermost and
// Discord (/slack suffix) incoming webhooks accept.
type Webhook struct{ URL string }

func (w Webhook) Notify(ctx context.Context, e Event) error {
	if w.URL == "" {
		return nil
	}
	status, _, err := postJSON(ctx, w.URL, map[string]string{"text": e.text() + "\n<" + e.URL + ">"})
	if err != nil {
		return errors.New("webhook: request failed")
	}
	if status >= 300 {
		return fmt.Errorf("webhook: status %d", status)
	}
	return nil
}

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"
)

// The model is optional: without MODEL_BASE_URL the gateway answers from tool results and
// spends nothing. With it, the model chooses the tool — it never decides whether it may run it.
type ModelTurn struct {
	Content          string
	ToolCalls        []ToolCall
	PromptTokens     int
	CompletionTokens int
}

func ModelEnabled() bool { return os.Getenv("MODEL_BASE_URL") != "" }

func CompleteWithTools(system, user string, schemas []map[string]any) (ModelTurn, error) {
	if !ModelEnabled() {
		return ModelTurn{}, errors.New("model_disabled")
	}
	base := os.Getenv("MODEL_BASE_URL")
	for len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	body, _ := json.Marshal(map[string]any{
		"model":       envOr("MODEL_NAME", "gpt-4o-mini"),
		"temperature": 0.2,
		"messages": []map[string]any{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"tools": schemas,
	})
	req, err := http.NewRequest(http.MethodPost, base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ModelTurn{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+os.Getenv("MODEL_API_KEY"))
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return ModelTurn{}, err
	}
	defer resp.Body.Close()
	var parsed struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return ModelTurn{}, err
	}
	if len(parsed.Choices) == 0 {
		return ModelTurn{}, errors.New("empty_choices")
	}
	turn := ModelTurn{PromptTokens: parsed.Usage.PromptTokens,
		CompletionTokens: parsed.Usage.CompletionTokens,
		Content:          parsed.Choices[0].Message.Content}
	for _, c := range parsed.Choices[0].Message.ToolCalls {
		args := map[string]any{}
		if c.Function.Arguments != "" {
			_ = json.Unmarshal([]byte(c.Function.Arguments), &args)
		}
		turn.ToolCalls = append(turn.ToolCalls, ToolCall{Name: c.Function.Name, Args: args})
	}
	return turn, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

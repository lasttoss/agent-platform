package main

// Types that cross the gateway boundary. The tools themselves live behind MCP now:
// this file holds no implementation, by design.
type SalaryRecord struct {
	Amount   int    `json:"amount"`
	Currency string `json:"currency"`
}

type ToolCall struct {
	Name string
	Args map[string]any
}

type ModelTurn struct {
	Content          string
	ToolCalls        []ToolCall
	PromptTokens     int
	CompletionTokens int
}

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

type assistantLine struct {
	Type    string `json:"type"`
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage      json.RawMessage `json:"usage"`
		StopReason string          `json:"stop_reason"`
	} `json:"message"`
}

// lastAssistantText returns the concatenated text blocks of the final
// assistant line in a JSONL transcript, plus its usage object and stop_reason.
func lastAssistantText(path string) (string, json.RawMessage, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil, "", fmt.Errorf("open transcript: %w", err)
	}
	defer func() { _ = f.Close() }()

	var lastText string
	var lastUsage json.RawMessage
	var lastStop string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		var al assistantLine
		if err := json.Unmarshal(line, &al); err != nil || al.Type != "assistant" {
			continue
		}
		text := ""
		for _, c := range al.Message.Content {
			if c.Type == "text" {
				text += c.Text
			}
		}
		lastUsage, lastStop = al.Message.Usage, al.Message.StopReason
		if text != "" {
			lastText = text
		}
	}
	if err := sc.Err(); err != nil {
		return "", nil, "", fmt.Errorf("scan transcript: %w", err)
	}
	return lastText, lastUsage, lastStop, nil
}

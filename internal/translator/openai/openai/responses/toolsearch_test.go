package responses

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

// tool_search declared in the Responses request must survive the conversion as
// a callable Chat Completions function.
func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_KeepsToolSearchAsFunction(t *testing.T) {
	raw := []byte(`{
		"tools": [{"type":"tool_search"}],
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"find Teams tools"}]}
		]
	}`)
	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("deepseek-v4.1-flash", raw, true)

	tools := gjson.GetBytes(out, "tools")
	if !tools.Exists() || !tools.IsArray() {
		t.Fatalf("tools must survive conversion: %s", out)
	}
	arr := tools.Array()
	if len(arr) != 1 {
		t.Fatalf("tools length = %d, want 1", len(arr))
	}
	if got := gjson.GetBytes(out, "tools.0.function.name").String(); got != "tool_search" {
		t.Fatalf("tools.0.function.name = %q, want %q", got, "tool_search")
	}
	if got := gjson.GetBytes(out, "tools.0.function.parameters.properties.query.type").String(); got != "string" {
		t.Fatalf("query parameter must exist: %s", out)
	}
}

// A completed tool_search round trip in history must convert to an assistant
// tool_calls entry plus a tool result message.
func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_ToolSearchRoundTripHistory(t *testing.T) {
	raw := []byte(`{
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"find Teams tools"}]},
			{"type":"tool_search_call","call_id":"call_search1","arguments":{"query":"Microsoft Teams"}},
			{"type":"tool_search_output","call_id":"call_search1","tools":[{"type":"namespace","name":"mcp__codex_apps__microsoft_teams","tools":[{"type":"function","name":"_list_chats"}]}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"now list the chats"}]}
		]
	}`)
	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("deepseek-v4.1-flash", raw, true)

	msgs := gjson.GetBytes(out, "messages")
	if !msgs.Exists() {
		t.Fatalf("messages missing: %s", out)
	}
	arr := msgs.Array()
	// Expect: user, assistant(tool_calls), tool, user
	if len(arr) < 4 {
		t.Fatalf("messages count = %d, want >= 4: %s", len(arr), out)
	}
	if got := gjson.GetBytes(out, "messages.1.role").String(); got != "assistant" {
		t.Fatalf("messages.1.role = %q, want assistant", got)
	}
	if got := gjson.GetBytes(out, "messages.1.tool_calls.0.function.name").String(); got != "tool_search" {
		t.Fatalf("messages.1.tool_calls.0 name = %q, want tool_search", got)
	}
	if got := gjson.GetBytes(out, "messages.2.role").String(); got != "tool" {
		t.Fatalf("messages.2.role = %q, want tool", got)
	}
	if got := gjson.GetBytes(out, "messages.2.tool_call_id").String(); got != "call_search1" {
		t.Fatalf("messages.2.tool_call_id = %q, want call_search1", got)
	}
	// The final user request must be preserved after the tool result.
	last := arr[len(arr)-1]
	if got := last.Get("role").String(); got != "user" {
		t.Fatalf("last message role = %q, want user", got)
	}
}

// The streamed Chat Completions tool_call named tool_search must surface back
// to the Codex client as a proprietary tool_search_call output item.
func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_ToolSearchCall(t *testing.T) {
	raw := []byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_search2\",\"type\":\"function\",\"function\":{\"name\":\"tool_search\",\"arguments\":\"\"}}]}}]}\n\ndata: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"query\\\":\\\"Microsoft Teams\\\"}\"}}]}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")

	var param any
	var allOutput [][]byte
	for _, chunk := range splitSSEChunks(raw) {
		allOutput = append(allOutput, ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "deepseek-v4.1-flash", []byte(`{}`), []byte(`{}`), chunk, &param)...)
	}
	out := allOutput
	if len(out) == 0 {
		t.Fatalf("no output produced")
	}
	foundCall := false
	foundDone := false
	for _, chunk := range allOutput {
		eventType := gjson.GetBytes(chunk, "type").String()
		switch eventType {
		case "response.output_item.done":
			if gjson.GetBytes(chunk, "item.type").String() == "tool_search_call" {
				foundCall = true
				if gjson.GetBytes(chunk, "item.arguments.query").String() != "Microsoft Teams" {
					t.Fatalf("tool_search_call arguments = %s", chunk)
				}
			}
		case "response.completed":
			items := gjson.GetBytes(chunk, "response.output").Array()
			for _, it := range items {
				if it.Get("type").String() == "tool_search_call" {
					foundDone = true
				}
			}
		}
	}
	if !foundCall {
		t.Fatalf("expected tool_search_call output_item.done event, output:\n%s", out)
	}
	if !foundDone {
		t.Fatalf("expected tool_search_call in response.completed output")
	}
}

func flattenChunks(chunks [][]byte) []byte {
	var all []byte
	for _, c := range chunks {
		all = append(all, c...)
	}
	return all
}

func splitSSEChunks(raw []byte) [][]byte {
	var chunks [][]byte
	for _, line := range bytesSplitLines(raw) {
		if len(line) > 6 && string(line[:6]) == "data: " {
			chunks = append(chunks, line[6:])
		}
	}
	return chunks
}

func bytesSplitLines(raw []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\n' {
			lines = append(lines, raw[start:i])
			start = i + 1
		}
	}
	if start < len(raw) {
		lines = append(lines, raw[start:])
	}
	return lines
}

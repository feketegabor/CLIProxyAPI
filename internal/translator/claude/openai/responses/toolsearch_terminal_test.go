package responses

import (
	"context"
	"fmt"
	"testing"
)

// A truncated discovery call must not become a completed client action after
// upstream's deferred tool finalization is combined with the discovery bridge.
func TestToolSearchTerminalStatus(t *testing.T) {
	for _, tc := range []struct{ stop, status, event string }{
		{"tool_use", "completed", "response.completed"},
		{"max_tokens", "incomplete", "response.incomplete"},
	} {
		t.Run(tc.stop, func(t *testing.T) {
			request := []byte(`{"tools":[{"type":"tool_search","execution":"client"}]}`)
			chunks := []string{
				`{"type":"message_start","message":{"id":"msg_search","usage":{"input_tokens":1,"output_tokens":0}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_search","name":"tool_search","input":{}}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"calendar\"}"}}`,
				`{"type":"content_block_stop","index":0}`,
				fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":1}}`, tc.stop),
				`{"type":"message_stop"}`,
			}
			var state any
			done, terminal := 0, 0
			for _, chunk := range chunks {
				for _, output := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", request, request, []byte("data: "+chunk), &state) {
					event, data := parseClaudeResponsesSSEEvent(t, output)
					if event == "response.output_item.done" {
						done++
						if data.Get("item.type").String() != "tool_search_call" || data.Get("item.status").String() != tc.status {
							t.Fatalf("incorrect discovery completion: %s", data.Raw)
						}
					}
					if event == tc.event {
						terminal++
						item := data.Get("response.output.0")
						if item.Get("type").String() != "tool_search_call" || item.Get("status").String() != tc.status || item.Get("call_id").String() != "call_search" {
							t.Fatalf("incorrect terminal discovery item: %s", item.Raw)
						}
					}
				}
			}
			if done != 1 || terminal != 1 {
				t.Fatalf("got %d item completions and %d terminal responses", done, terminal)
			}
		})
	}
}

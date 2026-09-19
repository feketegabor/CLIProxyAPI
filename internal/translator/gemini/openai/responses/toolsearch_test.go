package responses

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

// The proprietary tool_search declaration must survive the Gemini conversion as
// a callable functionDeclaration with the query argument.
func TestConvertOpenAIResponsesRequestToGemini_KeepsToolSearchAsFunction(t *testing.T) {
	raw := []byte(`{
		"tools": [{"type":"tool_search"}],
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"find Teams tools"}]}
		]
	}`)
	out := ConvertOpenAIResponsesRequestToGemini("gemini-3.8-flash-high", raw, true)

	decls := gjson.GetBytes(out, "tools.0.functionDeclarations")
	if !decls.Exists() || !decls.IsArray() {
		t.Fatalf("functionDeclarations must survive conversion: %s", out)
	}
	if got := gjson.GetBytes(out, `tools.0.functionDeclarations.0.name`).String(); got != "tool_search" {
		t.Fatalf("functionDeclarations.0.name = %q, want %q", got, "tool_search")
	}
	if got := gjson.GetBytes(out, `tools.0.functionDeclarations.0.parametersJsonSchema.properties.query.type`).String(); got != "string" {
		t.Fatalf("query parameter must exist: %s", out)
	}
}

// A completed tool_search round trip in history must replay as a standard
// Gemini functionCall/functionResponse pair, and the harvested tools must be
// pre-registered as callable functionDeclarations.
func TestConvertOpenAIResponsesRequestToGemini_ToolSearchRoundTripHistory(t *testing.T) {
	raw := []byte(`{
		"tools": [{"type":"tool_search"}],
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"find Teams tools"}]},
			{"type":"tool_search_call","call_id":"call_search1","arguments":{"query":"Microsoft Teams"}},
			{"type":"tool_search_output","call_id":"call_search1","tools":[{"type":"namespace","name":"mcp__codex_apps__microsoft_teams","tools":[{"type":"function","name":"_list_chats"}]}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"now list the chats"}]}
		]
	}`)
	out := ConvertOpenAIResponsesRequestToGemini("gemini-3.8-flash-high", raw, true)

	// The harvested Teams tool must be callable on the upstream.
	found := false
	for _, decl := range gjson.GetBytes(out, "tools.0.functionDeclarations").Array() {
		if decl.Get("name").String() == "mcp__codex_apps__microsoft_teams___list_chats" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("harvested tool must be declared: %s", out)
	}

	// History must carry the functionCall for tool_search and a functionResponse.
	var sawCall, sawResponse bool
	for _, content := range gjson.GetBytes(out, "contents").Array() {
		for _, part := range content.Get("parts").Array() {
			if part.Get("functionCall.name").String() == "tool_search" {
				sawCall = true
				if part.Get("functionCall.args.query").String() != "Microsoft Teams" {
					t.Fatalf("tool_search call args must carry the query: %s", part.Raw)
				}
			}
			if part.Get("functionResponse.name").String() == "tool_search" {
				sawResponse = true
			}
		}
	}
	if !sawCall || !sawResponse {
		t.Fatalf("tool_search round trip must replay as functionCall/functionResponse (call=%v response=%v): %s", sawCall, sawResponse, out)
	}
}

// The streamed flat tool_search function_call must surface to the ChatGPT
// client as the proprietary tool_search_call item with execution "client".
func TestConvertGeminiResponseToOpenAIResponses_ToolSearchCallRewrite(t *testing.T) {
	request := []byte(`{
		"model":"gemini-3.8-flash-high",
		"tools":[{"type":"tool_search"}],
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"find Teams tools"}]}]
	}`)
	upstream := []byte(`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"tool_search","args":{"query":"Microsoft Teams"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`)

	events := ConvertGeminiResponseToOpenAIResponses(context.Background(), "gemini-3.8-flash-high", request, request, upstream, new(any))
	var sawToolSearchCall, sawFunctionCall, sawDoneArgs bool
	for _, ev := range events {
		payload := gjson.GetBytes(ev, "item")
		if !payload.Exists() {
			continue
		}
		switch payload.Get("type").String() {
		case "tool_search_call":
			sawToolSearchCall = true
			if payload.Get("execution").String() != "client" {
				t.Fatalf("tool_search_call must carry execution=client: %s", payload.Raw)
			}
			if payload.Get("name").Exists() {
				t.Fatalf("tool_search_call must not carry a name field: %s", payload.Raw)
			}
			if payload.Get("status").String() == "completed" {
				if payload.Get("arguments.query").String() != "Microsoft Teams" {
					t.Fatalf("completed tool_search_call arguments must be an object with the query: %s", payload.Raw)
				}
				sawDoneArgs = true
			}
		case "function_call":
			if payload.Get("name").String() == "tool_search" {
				sawFunctionCall = true
			}
		}
	}
	if !sawToolSearchCall {
		t.Fatalf("tool_search_call item missing: %v", events)
	}
	if sawFunctionCall {
		t.Fatalf("flat tool_search function_call leaked to the client")
	}
	if !sawDoneArgs {
		t.Fatalf("completed tool_search_call item missing: %v", events)
	}
}

// The response.completed aggregate output must carry the converted item too.
func TestConvertGeminiResponseToOpenAIResponses_CompletedOutputRewrite(t *testing.T) {
	request := []byte(`{
		"model":"gemini-3.8-flash-high",
		"tools":[{"type":"tool_search"}],
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"find Teams tools"}]}]
	}`)
	upstream := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"tool_search","args":{"query":"Microsoft Teams"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`)

	resp := ConvertGeminiResponseToOpenAIResponsesNonStream(context.Background(), "gemini-3.8-flash-high", request, request, upstream, nil)
	found := false
	for _, item := range gjson.GetBytes(resp, "output").Array() {
		if item.Get("type").String() == "tool_search_call" {
			found = true
			if item.Get("execution").String() != "client" {
				t.Fatalf("non-stream tool_search_call must carry execution=client: %s", item.Raw)
			}
		}
		if item.Get("type").String() == "function_call" && item.Get("name").String() == "tool_search" {
			t.Fatalf("non-stream flat tool_search function_call leaked: %s", item.Raw)
		}
	}
	if !found {
		t.Fatalf("non-stream output missing tool_search_call: %s", resp)
	}
}

// Gemini truncates long qualified tool names to their local suffix; the
// namespace must be restored from the declarations that produced it.
func TestConvertGeminiResponseToOpenAIResponses_TruncatedNameRestoresNamespace(t *testing.T) {
	request := []byte(`{
		"model":"gemini-3.8-flash-high",
		"input":[
			{"type":"tool_search_output","call_id":"call_search1","tools":[{"type":"namespace","name":"mcp__codex_apps__microsoft_teams","tools":[{"type":"function","name":"_list_chats","description":"List chats","parameters":{"type":"object","properties":{}}}]}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"list chats"}]}
		]
	}`)
	upstream := []byte(`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"_list_chats","args":{}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`)

	events := ConvertGeminiResponseToOpenAIResponses(context.Background(), "gemini-3.8-flash-high", request, request, upstream, new(any))
	found := false
	for _, ev := range events {
		item := gjson.GetBytes(ev, "item")
		if item.Get("type").String() == "function_call" && item.Get("name").String() == "_list_chats" {
			found = true
			if ns := item.Get("namespace").String(); ns != "mcp__codex_apps__microsoft_teams" {
				t.Fatalf("namespace must be restored: %s", item.Raw)
			}
		}
	}
	if !found {
		t.Fatalf("function_call item missing: %v", events)
	}
}

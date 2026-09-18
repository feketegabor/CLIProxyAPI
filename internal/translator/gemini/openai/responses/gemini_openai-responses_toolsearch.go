package responses

import (
	"encoding/json"
	"strings"

	translatorcommon "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// geminiResponsesToolSearchSynthetic is the callable function that replaces the
// proprietary {"type":"tool_search"} declaration for Gemini and Antigravity
// upstreams. Those providers only accept standard function declarations, so
// the ChatGPT app-connector discovery handshake is replayed as an ordinary
// function round trip. The response-side rewrite
// (rewriteGeminiResponsesToolSearchEvents) converts the model's function call
// back into the proprietary tool_search_call item the client executes locally.
const geminiResponsesToolSearchSynthetic = `{"type":"function","name":"tool_search","description":"Search for and activate tools from installed apps (e.g. Microsoft Teams, Gmail, Google Drive, Outlook, Jira, GitHub). Always call this tool when the task requires interacting with an external app or service.","parameters":{"type":"object","properties":{"query":{"type":"string","description":"The name of the app or keywords of what you need, e.g. \"Microsoft Teams\" or \"Gmail\""}},"required":["query"]}}`

const geminiResponsesToolSearchCallIDFallback = "call_gemini_tool_search"

// bridgeToolSearchForGemini adapts the OpenAI proprietary tool_search handshake
// for Gemini-format upstreams:
//
//  1. The native tool_search declaration becomes a callable function tool
//     (util.BuildGeminiFunctionDeclarations converts it to a Gemini
//     functionDeclaration with the synthesized query parameter).
//  2. A completed handshake in the input history (tool_search_call plus
//     tool_search_output) becomes a standard function_call/function_call_output
//     pair. The tool_search_output item itself is preserved so the harvested
//     tool declarations keep registering on the upstream tools list.
func bridgeToolSearchForGemini(rawJSON []byte) []byte {
	hasToolSearch := false
	if tools := gjson.GetBytes(rawJSON, "tools"); tools.IsArray() {
		for _, tool := range tools.Array() {
			if tool.Get("type").String() == "tool_search" {
				hasToolSearch = true
				break
			}
		}
	}
	if input := gjson.GetBytes(rawJSON, "input"); input.IsArray() {
		for _, item := range input.Array() {
			itemType := item.Get("type").String()
			if itemType == "tool_search_call" || itemType == "tool_search_output" {
				hasToolSearch = true
				break
			}
		}
	}
	if !hasToolSearch {
		return rawJSON
	}

	if tools := gjson.GetBytes(rawJSON, "tools"); tools.IsArray() {
		rebuilt := make([][]byte, 0, len(tools.Array()))
		changed := false
		for _, tool := range tools.Array() {
			if tool.Get("type").String() == "tool_search" {
				changed = true
				rebuilt = append(rebuilt, []byte(geminiResponsesToolSearchSynthetic))
				continue
			}
			rebuilt = append(rebuilt, []byte(tool.Raw))
		}
		if changed {
			if updated, errSet := sjson.SetRawBytes(rawJSON, "tools", translatorcommon.JoinRawArray(rebuilt)); errSet == nil {
				rawJSON = updated
			}
		}
	}

	if input := gjson.GetBytes(rawJSON, "input"); input.IsArray() {
		rebuilt := make([][]byte, 0, len(input.Array())+8)
		changed := false
		for _, item := range input.Array() {
			switch item.Get("type").String() {
			case "tool_search_call":
				changed = true
				callID := item.Get("call_id").String()
				if callID == "" {
					callID = geminiResponsesToolSearchCallIDFallback
				}
				call, _ := sjson.SetBytes([]byte(`{"type":"function_call","call_id":"","name":"tool_search","arguments":"{}"}`), "call_id", callID)
				if arguments := item.Get("arguments"); arguments.Exists() {
					if arguments.Type == gjson.String && arguments.String() != "" {
						call, _ = sjson.SetBytes(call, "arguments", arguments.String())
					} else {
						call, _ = sjson.SetBytes(call, "arguments", arguments.Raw)
					}
				}
				rebuilt = append(rebuilt, call)
			case "tool_search_output":
				changed = true
				callID := item.Get("call_id").String()
				if callID == "" {
					callID = geminiResponsesToolSearchCallIDFallback
				}
				output, _ := sjson.SetBytes([]byte(`{"type":"function_call_output","call_id":"","output":""}`), "call_id", callID)
				output, _ = sjson.SetBytes(output, "output", `{"status":"success","tools_loaded":true}`)
				// The function result the model sees, followed by the original
				// item so its harvested tool declarations stay registered.
				rebuilt = append(rebuilt, output)
				rebuilt = append(rebuilt, []byte(item.Raw))
			default:
				rebuilt = append(rebuilt, []byte(item.Raw))
			}
		}
		if changed {
			if updated, errSet := sjson.SetRawBytes(rawJSON, "input", translatorcommon.JoinRawArray(rebuilt)); errSet == nil {
				rawJSON = updated
			}
		}
	}

	return rawJSON
}

// rewriteGeminiResponsesToolSearchFunctionCallItem converts one Responses
// function_call item back into the proprietary tool_search_call item the
// ChatGPT client executes locally. Items that are not a flat tool_search call
// are returned unchanged.
func rewriteGeminiResponsesToolSearchFunctionCallItem(item []byte) []byte {
	if gjson.GetBytes(item, "type").String() != "function_call" {
		return item
	}
	if name := gjson.GetBytes(item, "name").String(); name != "tool_search" || gjson.GetBytes(item, "namespace").String() != "" {
		return item
	}
	callID := gjson.GetBytes(item, "call_id").String()
	out, _ := sjson.SetBytes(item, "type", "tool_search_call")
	if callID != "" {
		out, _ = sjson.SetBytes(out, "id", "tsc_"+callID)
	}
	if status := gjson.GetBytes(item, "status").String(); status != "" {
		out, _ = sjson.SetBytes(out, "status", status)
	}
	out, _ = sjson.SetBytes(out, "execution", "client")
	out, _ = sjson.DeleteBytes(out, "name")
	// arguments must be an object for tool_search_call
	args := gjson.GetBytes(item, "arguments")
	if args.Type == gjson.String && args.String() != "" && gjson.Valid(args.String()) {
		out, _ = sjson.SetRawBytes(out, "arguments", []byte(args.String()))
	} else {
		out, _ = sjson.DeleteBytes(out, "arguments")
	}
	return out
}

// rewriteGeminiResponsesToolSearchEvents reverses the tool_search bridge on a
// batch of already-encoded Responses SSE events: flat tool_search function
// call items become proprietary tool_search_call items with execution "client",
// and the argument delta/done events of those items are dropped so the client
// only observes the single converted item.
func rewriteGeminiResponsesToolSearchEvents(out [][]byte) [][]byte {
	if len(out) == 0 {
		return out
	}
	toolSearchItemIDs := make(map[string]bool)
	changed := false
	for i := range out {
		event := out[i]
		payload := extractSSEJSONPayload(event)
		if len(payload) == 0 {
			continue
		}
		root := gjson.ParseBytes(payload)
		eventType := root.Get("type").String()
		switch eventType {
		case "response.output_item.added", "response.output_item.done":
			item := root.Get("item")
			if item.Get("type").String() != "function_call" {
				continue
			}
			if name := item.Get("name").String(); name != "tool_search" || item.Get("namespace").String() != "" {
				continue
			}
			rewritten, errSet := sjson.SetRawBytes(payload, "item", rewriteGeminiResponsesToolSearchFunctionCallItem([]byte(item.Raw)))
			if errSet != nil {
				continue
			}
			callID := item.Get("call_id").String()
			if callID != "" {
				// The argument delta/done events still carry the original
				// "fc_"-prefixed item id, so track both identities.
				toolSearchItemIDs["fc_"+callID] = true
				toolSearchItemIDs[callID] = true
			}
			if id := gjson.GetBytes(rewritten, "item.id").String(); id != "" {
				toolSearchItemIDs[id] = true
			}
			out[i] = translatorcommon.SSEEventData(eventType, rewritten)
			changed = true
		case "response.function_call_arguments.delta", "response.function_call_arguments.done":
			// Items may be tracked from an earlier pass in this batch or a
			// previous chunk (state does not persist across chunks, but Gemini
			// emits the whole call in one chunk).
			if toolSearchItemIDs[root.Get("item_id").String()] {
				out[i] = nil
				changed = true
			}
		case "response.completed", "response.incomplete", "response.in_progress", "response.created":
			output := root.Get("response.output")
			if !output.IsArray() {
				continue
			}
			outputChanged := false
			rewritten := make([]json.RawMessage, 0, len(output.Array()))
			for _, it := range output.Array() {
				after := rewriteGeminiResponsesToolSearchFunctionCallItem([]byte(it.Raw))
				if string(after) != it.Raw {
					outputChanged = true
					if id := gjson.GetBytes(after, "id").String(); id != "" {
						toolSearchItemIDs[id] = true
					}
					if callID := gjson.GetBytes(after, "call_id").String(); callID != "" {
						toolSearchItemIDs[callID] = true
					}
				}
				rewritten = append(rewritten, json.RawMessage(after))
			}
			if !outputChanged {
				continue
			}
			joined, errMarshal := json.Marshal(rewritten)
			if errMarshal != nil {
				continue
			}
			updated, errSet := sjson.SetRawBytes(payload, "response.output", joined)
			if errSet != nil {
				continue
			}
			out[i] = translatorcommon.SSEEventData(eventType, updated)
			changed = true
		}
	}
	if !changed {
		return out
	}
	filtered := make([][]byte, 0, len(out))
	for _, event := range out {
		if event == nil {
			continue
		}
		filtered = append(filtered, event)
	}
	return filtered
}

// rewriteGeminiResponsesToolSearchNonStream reverses the tool_search bridge in
// a non-streaming Responses JSON payload's output array.
func rewriteGeminiResponsesToolSearchNonStream(resp []byte) []byte {
	output := gjson.GetBytes(resp, "output")
	if !output.IsArray() {
		return resp
	}
	changed := false
	rewritten := make([]json.RawMessage, 0, len(output.Array()))
	for _, it := range output.Array() {
		after := rewriteGeminiResponsesToolSearchFunctionCallItem([]byte(it.Raw))
		if string(after) != it.Raw {
			changed = true
		}
		rewritten = append(rewritten, json.RawMessage(after))
	}
	if !changed {
		return resp
	}
	joined, errMarshal := json.Marshal(rewritten)
	if errMarshal != nil {
		return resp
	}
	updated, errSet := sjson.SetRawBytes(resp, "output", joined)
	if errSet != nil {
		return resp
	}
	return updated
}

func bytesHasDataPrefix(b []byte) bool {
	return len(b) >= 6 && string(b[:6]) == "data: "
}

// extractSSEJSONPayload returns the JSON payload of one encoded SSE event:
// "event: <name>\ndata: <json>". It returns an empty slice when the event has
// no data line.
func extractSSEJSONPayload(event []byte) []byte {
	idx := bytesIndex(event, []byte("\ndata: "))
	if idx >= 0 {
		return bytesTrimSpace(event[idx+len("\ndata: "):])
	}
	if bytesHasDataPrefix(event) {
		return bytesTrimSpace(event[len("data: "):])
	}
	return nil
}

// bytesIndex reports the index of the first instance of sub in b, or -1.
func bytesIndex(b, sub []byte) int {
	return strings.Index(string(b), string(sub))
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

package responses

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	translatorcommon "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// codexToolSearchStreamState tracks per-response stream state needed to
// reverse the tool_search bridge applied by ConvertOpenAIResponsesRequestToCodex.
type codexToolSearchState struct {
	toolSearchItemIDs map[string]bool
	qualifiedTools    map[string]codexQualifiedTool
}

type codexQualifiedTool struct {
	localName string
	namespace string
}

// buildCodexQualifiedToolMap collects every namespace tool declaration of the
// original client request (top-level tools, additional_tools input items and
// tools resolved by a completed tool_search handshake) so streamed function
// calls using qualified names can be reported back to the client with their
// owning namespace restored.
func buildCodexQualifiedTools(requestRawJSON []byte) map[string]codexQualifiedTool {
	qualified := make(map[string]codexQualifiedTool)
	scan := func(toolsResult gjson.Result) {
		if !toolsResult.IsArray() {
			return
		}
		for _, tool := range toolsResult.Array() {
			if strings.TrimSpace(tool.Get("type").String()) != "namespace" {
				continue
			}
			namespaceName := strings.TrimSpace(tool.Get("name").String())
			if children := tool.Get("tools"); children.IsArray() {
				for _, child := range children.Array() {
					localName := child.Get("name").String()
					if localName == "" {
						continue
					}
					name := localName
					if namespaceName != "" && !strings.HasPrefix(localName, "mcp__") && !strings.HasPrefix(localName, namespaceName) {
						name = namespaceName + "__" + localName
					}
					if _, dup := qualified[name]; !dup {
						qualified[name] = codexQualifiedTool{localName: localName, namespace: namespaceName}
					}
				}
			}
		}
	}
	scan(gjson.GetBytes(requestRawJSON, "tools"))
	if input := gjson.GetBytes(requestRawJSON, "input"); input.IsArray() {
		for _, item := range input.Array() {
			itemType := item.Get("type").String()
			if itemType == "additional_tools" {
				scan(item.Get("tools"))
			}
			if itemType == "tool_search_output" {
				scan(item.Get("tools"))
			}
		}
	}
	return qualified
}

// rewriteCodexOutputItem reverses the tool_search bridge for one streamed
// function_call item: the flat tool_search call becomes the proprietary
// tool_search_call item the Codex client executes locally, and qualified
// function names get their namespace restored.
func rewriteCodexOutputItem(item gjson.Result, st *codexToolSearchState) []byte {
	if strings.TrimSpace(item.Get("type").String()) != "function_call" {
		return []byte(item.Raw)
	}
	name := item.Get("name").String()
	namespace := item.Get("namespace").String()
	if namespace == "" {
		if name == "tool_search" {
			out := []byte(item.Raw)
			callID := item.Get("call_id").String()
			out, _ = sjson.SetBytes(out, "type", "tool_search_call")
			out, _ = sjson.SetBytes(out, "id", "tsc_"+callID)
			out, _ = sjson.SetBytes(out, "status", item.Get("status").String())
			out, _ = sjson.SetBytes(out, "execution", "client")
			out, _ = sjson.DeleteBytes(out, "name")
			// arguments must be an object for tool_search_call
			args := item.Get("arguments")
			if args.Type == gjson.String && args.String() != "" && gjson.Valid(args.String()) {
				out, _ = sjson.SetRawBytes(out, "arguments", []byte(args.String()))
			} else if args.Exists() {
				out, _ = sjson.DeleteBytes(out, "arguments")
			}
			if st != nil && callID != "" {
				if st.toolSearchItemIDs == nil {
					st.toolSearchItemIDs = make(map[string]bool)
				}
				st.toolSearchItemIDs[item.Get("id").String()] = true
				st.toolSearchItemIDs[callID] = true
			}
			return out
		}
		if qt, ok := st.qualifiedTools[name]; ok {
			out, _ := sjson.SetBytes([]byte(item.Raw), "name", qt.localName)
			out, _ = sjson.SetBytes(out, "namespace", qt.namespace)
			return out
		}
	}
	return []byte(item.Raw)
}

// ConvertCodexResponseToOpenAIResponses converts Codex (Responses-native)
// upstream SSE chunks to OpenAI Responses SSE events for the client, reversing
// the tool_search bridge applied on the request path.
func ConvertCodexResponseToOpenAIResponses(_ context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte {
	var localState codexToolSearchState
	st := &localState
	if param != nil {
		if *param == nil {
			*param = st
		}
		st = (*param).(*codexToolSearchState)
	}
	st.qualifiedTools = buildCodexQualifiedTools(originalRequestRawJSON)

	rest := rawJSON
	if bytes.HasPrefix(rest, []byte("data:")) {
		rest = bytes.TrimSpace(rest[5:])
	}
	rest = bytes.TrimSpace(rest)
	if len(rest) == 0 || bytes.Equal(rest, []byte("[DONE]")) {
		return [][]byte{rawJSON}
	}
	root := gjson.ParseBytes(rest)
	eventType := root.Get("type").String()

	switch eventType {
	case "response.output_item.added", "response.output_item.done":
		item := root.Get("item")
		if item.Get("type").String() == "function_call" {
			rewritten := rewriteCodexOutputItem(item, st)
			updated, errSet := sjson.SetRawBytes(rest, "item", rewritten)
			if errSet == nil {
				return [][]byte{append([]byte("data: "), updated...)}
			}
		}
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		itemID := root.Get("item_id").String()
		if st.toolSearchItemIDs[itemID] {
			return [][]byte{}
		}
	case "response.completed", "response.incomplete":
		if output := root.Get("response.output"); output.IsArray() {
			changed := false
			var rewritten [][]byte
			for _, it := range output.Array() {
				before := it.Raw
				after := rewriteCodexOutputItem(it, st)
				if string(after) != before {
					changed = true
				}
				rewritten = append(rewritten, after)
			}
			if changed {
				joined, _ := json.Marshal(rewritten)
				updated, errSet := sjson.SetRawBytes(rest, "response.output", joined)
				if errSet == nil {
					return [][]byte{append([]byte("data: "), setResponsesModel(updated, modelName, originalRequestRawJSON, requestRawJSON)...)}
				}
			}
		}
	}

	return [][]byte{setResponsesModel(rawJSON, modelName, originalRequestRawJSON, requestRawJSON)}
}

// setResponsesModel ensures the streamed response.created / in_progress
// events carry the model name the client requested.
func setResponsesModel(rawJSON []byte, modelName string, originalRequestRawJSON, requestRawJSON []byte) []byte {
	eventType := gjson.GetBytes(rawJSON, "type").String()
	if eventType != "response.created" && eventType != "response.in_progress" {
		return rawJSON
	}
	if gjson.GetBytes(rawJSON, "response.model").Exists() {
		return rawJSON
	}

	requestModelName := translatorcommon.RequestModelName(originalRequestRawJSON, requestRawJSON)
	if requestModelName == "" {
		requestModelName = modelName
	}
	if requestModelName == "" {
		return rawJSON
	}

	updated, errSet := sjson.SetBytes(rawJSON, "response.model", requestModelName)
	if errSet != nil {
		return rawJSON
	}
	return updated
}

// ConvertCodexResponseToOpenAIResponsesNonStream builds a single Responses JSON
// from a non-streaming OpenAI Chat Completions response.
func ConvertCodexResponseToOpenAIResponsesNonStream(_ context.Context, _ string, _, _, rawJSON []byte, _ *any) []byte {
	rootResult := gjson.ParseBytes(rawJSON)
	// Verify this is a terminal response event.
	responseType := rootResult.Get("type").String()
	if responseType != "response.completed" && responseType != "response.incomplete" {
		return []byte{}
	}
	responseResult := rootResult.Get("response")
	return []byte(responseResult.Raw)
}

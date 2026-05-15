package service

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
)

func TestBuildCleanSessionArchiveRecordMinimalSchema(t *testing.T) {
	raw := &sessionArchiveRawRecord{
		RecordType:      sessionArchiveRecordType,
		RequestMethod:   "POST",
		IsStream:        true,
		OriginModelName: "gpt-test",
		UpstreamModel:   "gpt-test-upstream",
		RequestObject: map[string]any{
			"model":    "gpt-test",
			"stream":   true,
			"system":   "remove-me",
			"thinking": map[string]any{"type": "enabled"},
			"messages": []any{
				map[string]any{"role": "system", "content": "system text"},
				map[string]any{"role": "user", "content": "hello"},
			},
		},
		RequestBody:  `{"model":"gpt-test"}`,
		ResponseBody: `{"choices":[{"message":{"content":"world"}}],"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}`,
		ResponseText: "world",
		RequestHeaders: map[string]string{
			"Authorization": "secret",
			"User-Agent":    " claude-cli/1.0 (external, cli) ",
		},
		ResponseUsage: &SessionArchiveUsageRecord{
			InputTokens:  11,
			OutputTokens: 22,
			TotalTokens:  33,
		},
	}

	cleaned := buildCleanSessionArchiveRecord(raw)
	if cleaned == nil {
		t.Fatal("expected cleaned record")
	}
	if cleaned.UserAgent != " claude-cli/1.0 (external, cli) " {
		t.Fatalf("unexpected user_agent: %q", cleaned.UserAgent)
	}
	if cleaned.ResponseUsage == nil || cleaned.ResponseUsage.InputTokens != 11 || cleaned.ResponseUsage.OutputTokens != 22 || cleaned.ResponseUsage.TotalTokens != 33 {
		t.Fatalf("unexpected response_usage: %#v", cleaned.ResponseUsage)
	}

	requestObject, ok := cleaned.RequestObject.(map[string]any)
	if !ok {
		t.Fatalf("expected request_object map, got %T", cleaned.RequestObject)
	}
	assertRequestObjectOnlyAllowedFields(t, requestObject)
	messages := requestObjectMessages(t, requestObject)
	if len(messages) != 3 {
		t.Fatalf("expected user/system-normalized plus assistant response messages, got %#v", messages)
	}
	if hasAnyKey(requestObject, "model", "stream", "system", "thinking", "metadata", "max_tokens") {
		t.Fatalf("request_object contains forbidden fields: %#v", requestObject)
	}
	lastMessage := messages[len(messages)-1].(map[string]any)
	if lastMessage["role"] != "assistant" {
		t.Fatalf("expected appended assistant response, got %#v", lastMessage)
	}
	lastContent := lastMessage["content"].([]any)
	lastBlock := lastContent[0].(map[string]any)
	if lastBlock["type"] != "text" || lastBlock["text"] != "world" {
		t.Fatalf("unexpected assistant response block: %#v", lastBlock)
	}
}

func TestBuildCleanSessionArchiveRecordNormalizesMessagesAndTools(t *testing.T) {
	raw := &sessionArchiveRawRecord{
		RecordType:      sessionArchiveRecordType,
		OriginModelName: "claude-opus-4-6",
		UpstreamModel:   "claude-opus-4-6",
		RequestObject: map[string]any{
			"model":  "claude-opus-4-6",
			"stream": true,
			"tools": []any{
				map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":        "Bash",
						"description": "Run a command",
						"parameters":  map[string]any{"type": "object"},
					},
				},
			},
			"messages": []any{
				map[string]any{
					"role":    "user",
					"content": "hello",
				},
				map[string]any{
					"role":              "assistant",
					"reasoning_content": "drop me",
					"tool_calls": []any{
						map[string]any{
							"id": "call_1",
							"function": map[string]any{
								"name":      "Bash",
								"arguments": `{"command":"ls"}`,
							},
						},
					},
				},
				map[string]any{
					"role":         "tool",
					"tool_call_id": "call_1",
					"content":      "README.md",
				},
			},
		},
		ResponseText: "done",
	}

	cleaned := buildCleanSessionArchiveRecord(raw)
	requestObject := cleaned.RequestObject.(map[string]any)
	assertRequestObjectOnlyAllowedFields(t, requestObject)

	messages := requestObjectMessages(t, requestObject)
	if len(messages) != 4 {
		t.Fatalf("unexpected messages: %#v", messages)
	}
	firstBlock := firstContentBlock(t, messages[0])
	if firstBlock["type"] != "text" || firstBlock["text"] != "hello" {
		t.Fatalf("unexpected first content block: %#v", firstBlock)
	}

	toolUse := firstContentBlock(t, messages[1])
	if toolUse["type"] != "tool_use" || toolUse["id"] != "call_1" || toolUse["name"] != "Bash" {
		t.Fatalf("unexpected tool_use block: %#v", toolUse)
	}
	input, ok := toolUse["input"].(map[string]any)
	if !ok || input["command"] != "ls" {
		t.Fatalf("unexpected tool input: %#v", toolUse["input"])
	}
	if hasAnyKey(toolUse, "cache_control", "thinking", "signature") {
		t.Fatalf("tool_use contains forbidden fields: %#v", toolUse)
	}

	toolResultMessage := messages[2].(map[string]any)
	if toolResultMessage["role"] != "user" {
		t.Fatalf("unexpected tool result role: %#v", toolResultMessage)
	}
	toolResult := firstContentBlock(t, messages[2])
	if toolResult["type"] != "tool_result" || toolResult["tool_use_id"] != "call_1" || toolResult["content"] != "README.md" || toolResult["is_error"] != false {
		t.Fatalf("unexpected tool_result block: %#v", toolResult)
	}

	tools, ok := requestObject["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("unexpected tools: %#v", requestObject["tools"])
	}
	tool, ok := tools[0].(map[string]any)
	if !ok || tool["name"] != "Bash" || tool["description"] != "Run a command" || tool["input_schema"] == nil {
		t.Fatalf("unexpected normalized tool: %#v", tools[0])
	}
}

func TestAppendSessionArchiveRecordWritesOnlyAllowedTopLevelFields(t *testing.T) {
	tempDir := t.TempDir()
	originalDir := common.SessionArchiveDir
	common.SessionArchiveDir = tempDir
	defer func() {
		common.SessionArchiveDir = originalDir
	}()

	startedAt := time.Date(2026, 5, 13, 12, 0, 0, 0, time.Local)
	record := &SessionArchiveRecord{
		RecordType:      sessionArchiveRecordType,
		SessionKey:      "sid_stable",
		OriginModelName: "claude-opus-4-6",
		UpstreamModel:   "claude-opus-4-6",
		RequestObject: map[string]any{
			"model": "claude-opus-4-6",
			"messages": []any{
				map[string]any{"role": "user", "content": "hello"},
			},
		},
		RequestBody:    `{"model":"claude-opus-4-6"}`,
		ResponseBody:   `{"model":"claude-opus-4-6","content":"ok"}`,
		ResponseText:   "ok",
		RequestHeaders: map[string]string{"Authorization": "secret", "User-Agent": "unit-test/1.0"},
		ResponseUsage: &SessionArchiveUsageRecord{
			InputTokens:  4,
			OutputTokens: 5,
			TotalTokens:  9,
		},
	}
	if err := appendSessionArchiveRecord(record, "claude-opus-4-6", startedAt); err != nil {
		t.Fatalf("appendSessionArchiveRecord returned error: %v", err)
	}

	saved := readSingleArchiveLineAsMap(t, tempDir, "claude-opus-4-6", "session-20260513.jsonl")
	assertTopLevelOnlyAllowedFields(t, saved)
	if saved["record_type"] != sessionArchiveRecordType {
		t.Fatalf("unexpected record_type: %#v", saved["record_type"])
	}
	if saved["session_id"] != "sid_stable" {
		t.Fatalf("unexpected session_id: %#v", saved["session_id"])
	}
	if saved["user_agent"] != "unit-test/1.0" {
		t.Fatalf("unexpected user_agent: %#v", saved["user_agent"])
	}
	usage, ok := saved["response_usage"].(map[string]any)
	if !ok || usage["input_tokens"] != float64(4) || usage["output_tokens"] != float64(5) || usage["total_tokens"] != float64(9) {
		t.Fatalf("unexpected response_usage: %#v", saved["response_usage"])
	}
	requestObject, ok := saved["request_object"].(map[string]any)
	if !ok {
		t.Fatalf("expected request_object map, got %#v", saved["request_object"])
	}
	assertRequestObjectOnlyAllowedFields(t, requestObject)
	messages := requestObjectMessages(t, requestObject)
	if len(messages) != 2 {
		t.Fatalf("expected request plus assistant output, got %#v", messages)
	}
}

func TestAppendSessionArchiveRecordOmitsMissingUsage(t *testing.T) {
	tempDir := t.TempDir()
	originalDir := common.SessionArchiveDir
	common.SessionArchiveDir = tempDir
	defer func() {
		common.SessionArchiveDir = originalDir
	}()

	record := &SessionArchiveRecord{
		SessionKey:      "sid_no_usage",
		OriginModelName: "gpt-4o",
		UserAgent:       "unit-test/1.0",
		RequestObject: map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		},
		ResponseText: "ok",
	}
	if err := appendSessionArchiveRecord(record, "gpt-4o", time.Date(2026, 5, 13, 12, 0, 0, 0, time.Local)); err != nil {
		t.Fatalf("appendSessionArchiveRecord returned error: %v", err)
	}
	saved := readSingleArchiveLineAsMap(t, tempDir, "gpt-4o", "session-20260513.jsonl")
	assertTopLevelOnlyAllowedFields(t, saved)
	if _, exists := saved["response_usage"]; exists {
		t.Fatalf("expected response_usage to be omitted when upstream usage is missing: %#v", saved)
	}
}

func TestAppendSessionArchiveRecordUpsertsBySessionID(t *testing.T) {
	tempDir := t.TempDir()
	originalDir := common.SessionArchiveDir
	common.SessionArchiveDir = tempDir
	defer func() {
		common.SessionArchiveDir = originalDir
	}()

	startedAt := time.Date(2026, 5, 13, 12, 0, 0, 0, time.Local)
	first := archiveRecordForTest("sid_stable", "first", "first answer")
	second := archiveRecordForTest("sid_stable", "second", "second answer")

	if err := appendSessionArchiveRecord(first, "claude-opus-4-6", startedAt); err != nil {
		t.Fatalf("first append returned error: %v", err)
	}
	if err := appendSessionArchiveRecord(second, "claude-opus-4-6", startedAt); err != nil {
		t.Fatalf("second append returned error: %v", err)
	}

	lines := readArchiveLines(t, tempDir, "claude-opus-4-6", "session-20260513.jsonl")
	if len(lines) != 1 {
		t.Fatalf("expected one upserted line, got %d: %s", len(lines), strings.Join(lines, "\n"))
	}
	var saved map[string]any
	if err := common.Unmarshal([]byte(lines[0]), &saved); err != nil {
		t.Fatalf("failed to parse archive line: %v", err)
	}
	if saved["session_id"] != "sid_stable" {
		t.Fatalf("unexpected session_id: %#v", saved["session_id"])
	}
	if strings.Contains(lines[0], "first answer") || !strings.Contains(lines[0], "second answer") {
		t.Fatalf("expected latest record only, got %s", lines[0])
	}
	if strings.Contains(lines[0], "session_key") || strings.Contains(lines[0], "response_text") {
		t.Fatalf("line contains forbidden legacy fields: %s", lines[0])
	}
}

func TestAppendSessionArchiveRecordReusesFallbackKeyForMessagePrefix(t *testing.T) {
	tempDir := t.TempDir()
	originalDir := common.SessionArchiveDir
	common.SessionArchiveDir = tempDir
	defer func() {
		common.SessionArchiveDir = originalDir
	}()

	startedAt := time.Date(2026, 5, 13, 12, 0, 0, 0, time.Local)
	first := &SessionArchiveRecord{
		OriginModelName: "claude-opus-4-6",
		UpstreamModel:   "claude-opus-4-6",
		UserAgent:       "unit-test/1.0",
		RequestObject: map[string]any{
			"messages": []any{
				map[string]any{"role": "user", "content": "same root"},
			},
		},
		ResponseText: "first answer",
	}
	second := &SessionArchiveRecord{
		OriginModelName: "claude-opus-4-6",
		UpstreamModel:   "claude-opus-4-6",
		UserAgent:       "unit-test/1.0",
		RequestObject: map[string]any{
			"messages": []any{
				map[string]any{"role": "user", "content": "same root"},
				map[string]any{"role": "assistant", "content": "first answer"},
				map[string]any{"role": "user", "content": "next"},
			},
		},
		ResponseText: "second answer",
	}

	if err := appendSessionArchiveRecord(first, "claude-opus-4-6", startedAt); err != nil {
		t.Fatalf("first append returned error: %v", err)
	}
	firstSaved := readSingleArchiveLineAsMap(t, tempDir, "claude-opus-4-6", "session-20260513.jsonl")
	firstSessionID, ok := firstSaved["session_id"].(string)
	if !ok || firstSessionID == "" || strings.HasPrefix(firstSessionID, "sid_") {
		t.Fatalf("expected fallback ctx session_id, got %#v", firstSaved["session_id"])
	}

	if err := appendSessionArchiveRecord(second, "claude-opus-4-6", startedAt); err != nil {
		t.Fatalf("second append returned error: %v", err)
	}
	lines := readArchiveLines(t, tempDir, "claude-opus-4-6", "session-20260513.jsonl")
	if len(lines) != 1 {
		t.Fatalf("expected one upserted line, got %d: %s", len(lines), strings.Join(lines, "\n"))
	}
	var secondSaved map[string]any
	if err := common.Unmarshal([]byte(lines[0]), &secondSaved); err != nil {
		t.Fatalf("failed to parse second archive: %v", err)
	}
	if secondSaved["session_id"] != firstSessionID {
		t.Fatalf("expected reused session_id %q, got %#v", firstSessionID, secondSaved["session_id"])
	}
	if !strings.Contains(lines[0], "second answer") {
		t.Fatalf("expected latest response in archive, got %s", lines[0])
	}
}

func TestExplicitSessionArchiveKeyIgnoresRequestID(t *testing.T) {
	info := &relaycommon.RelayInfo{RequestId: "req-1"}
	if key := explicitSessionArchiveKey(info, "req-1"); key != "" {
		t.Fatalf("expected request_id not to be used as session key, got %q", key)
	}
	if key := explicitSessionArchiveKey(info, "sess-1"); key == "" || !strings.HasPrefix(key, "sid_") {
		t.Fatalf("expected explicit stable session key, got %q", key)
	}
}

func TestSessionArchiveKeySeedWithoutChannelMeta(t *testing.T) {
	info := &relaycommon.RelayInfo{OriginModelName: "gpt-test"}
	if got := sessionArchiveKeySeed(info); got != "gpt-test" {
		t.Fatalf("unexpected key seed without channel meta: %q", got)
	}
}

func TestStartSessionArchiveCaptureWithoutChannelMeta(t *testing.T) {
	originalEnabled := common.SessionArchiveEnabled
	common.SessionArchiveEnabled = true
	defer func() {
		common.SessionArchiveEnabled = originalEnabled
	}()

	common.OptionMapRWMutex.Lock()
	originalEnabledModels, hadEnabledModels := common.OptionMap[common.SessionArchiveEnabledModelsOptionKey]
	common.OptionMap[common.SessionArchiveEnabledModelsOptionKey] = `["gpt-test"]`
	common.OptionMapRWMutex.Unlock()
	defer func() {
		common.OptionMapRWMutex.Lock()
		if hadEnabledModels {
			common.OptionMap[common.SessionArchiveEnabledModelsOptionKey] = originalEnabledModels
		} else {
			delete(common.OptionMap, common.SessionArchiveEnabledModelsOptionKey)
		}
		common.OptionMapRWMutex.Unlock()
	}()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-test"}`))

	defer func() {
		if err := recover(); err != nil {
			t.Fatalf("StartSessionArchiveCapture should not panic: %v", err)
		}
	}()

	capture := StartSessionArchiveCapture(c, &relaycommon.RelayInfo{
		OriginModelName: "gpt-test",
		RequestHeaders:  map[string]string{"User-Agent": "unit-test/1.0"},
	}, nil, `{"model":"gpt-test","messages":[{"role":"user","content":"ping"}]}`)
	if capture == nil {
		t.Fatal("expected session archive capture")
	}
}

func TestShouldKeepSessionArchiveRecord(t *testing.T) {
	base := &sessionArchiveRawRecord{
		TurnComplete:       true,
		StreamComplete:     true,
		ResponseHTTPStatus: 200,
		RequestHeaders: map[string]string{
			"User-Agent": "claude-cli/1.0",
		},
		RequestObject: map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		},
		ResponseText: "ok",
	}
	if !shouldKeepSessionArchiveRecord(base) {
		t.Fatal("expected successful record to be kept")
	}

	for name, record := range map[string]*sessionArchiveRawRecord{
		"check-cx": {
			TurnComplete:       true,
			StreamComplete:     true,
			ResponseHTTPStatus: 200,
			RequestHeaders:     map[string]string{"User-Agent": "check-cx/1.0"},
			RequestObject:      base.RequestObject,
			ResponseText:       "ok",
		},
		"missing user-agent": {
			TurnComplete:       true,
			StreamComplete:     true,
			ResponseHTTPStatus: 200,
			RequestObject:      base.RequestObject,
			ResponseText:       "ok",
		},
		"incomplete": {
			TurnComplete:       false,
			StreamComplete:     true,
			ResponseHTTPStatus: 200,
			RequestHeaders:     map[string]string{"User-Agent": "claude-cli/1.0"},
			RequestObject:      base.RequestObject,
			ResponseText:       "ok",
		},
		"non-2xx": {
			TurnComplete:       true,
			StreamComplete:     true,
			ResponseHTTPStatus: 500,
			RequestHeaders:     map[string]string{"User-Agent": "claude-cli/1.0"},
			RequestObject:      base.RequestObject,
			ResponseText:       "ok",
		},
		"stream failed": {
			TurnComplete:       true,
			StreamComplete:     false,
			ResponseHTTPStatus: 200,
			RequestHeaders:     map[string]string{"User-Agent": "claude-cli/1.0"},
			RequestObject:      base.RequestObject,
			ResponseText:       "ok",
		},
		"no assistant output": {
			TurnComplete:       true,
			StreamComplete:     true,
			ResponseHTTPStatus: 200,
			RequestHeaders:     map[string]string{"User-Agent": "claude-cli/1.0"},
			RequestObject:      base.RequestObject,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if shouldKeepSessionArchiveRecord(record) {
				t.Fatal("expected record to be filtered")
			}
		})
	}
}

func TestSessionArchiveModelAlias(t *testing.T) {
	common.OptionMapRWMutex.Lock()
	originalMap := common.OptionMap
	common.OptionMap = map[string]string{
		common.SessionArchiveModelAliasesOptionKey: `{"claude-opus-4-7":"claude-opus-4-6","empty":" "}`,
	}
	common.OptionMapRWMutex.Unlock()
	defer func() {
		common.OptionMapRWMutex.Lock()
		common.OptionMap = originalMap
		common.OptionMapRWMutex.Unlock()
	}()

	if got := sessionArchiveModelAlias("claude-opus-4-7"); got != "claude-opus-4-6" {
		t.Fatalf("alias = %q, want claude-opus-4-6", got)
	}
	if got := sessionArchiveModelAlias("claude-opus-4-6"); got != "claude-opus-4-6" {
		t.Fatalf("fallback alias = %q, want original model", got)
	}
	if got := sessionArchiveModelAlias("empty"); got != "empty" {
		t.Fatalf("empty alias = %q, want original model", got)
	}
}

func TestAppendSessionArchiveRecordUsesArchiveModelPath(t *testing.T) {
	tempDir := t.TempDir()
	originalDir := common.SessionArchiveDir
	common.SessionArchiveDir = tempDir
	defer func() {
		common.SessionArchiveDir = originalDir
	}()

	startedAt := time.Date(2026, 5, 13, 12, 0, 0, 0, time.Local)
	record := archiveRecordForTest("sid_path", "hello", "ok")
	if err := appendSessionArchiveRecord(record, "claude-opus-4-6", startedAt); err != nil {
		t.Fatalf("appendSessionArchiveRecord returned error: %v", err)
	}

	expectedPath := filepath.Join(tempDir, "claude-opus-4-6", "session-20260513.jsonl")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Fatalf("expected archive at %s: %v", expectedPath, err)
	}
}

func TestWriteSessionArchiveDailySummary(t *testing.T) {
	tempDir := t.TempDir()
	originalDir := common.SessionArchiveDir
	common.SessionArchiveDir = tempDir
	defer func() {
		common.SessionArchiveDir = originalDir
	}()

	targetDay := time.Date(2026, 5, 12, 12, 0, 0, 0, time.Local)
	generatedAt := targetDay.Add(24 * time.Hour)
	modelAPath := sessionArchiveFilePath("gpt-4o", targetDay)
	if err := os.MkdirAll(filepath.Dir(modelAPath), 0755); err != nil {
		t.Fatalf("failed to create model directory: %v", err)
	}
	recordLines := []string{
		`{"record_type":"session_turn","session_id":"a","user_agent":"ua","response_usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30},"request_object":{"messages":[]}}`,
		`{"record_type":"session_turn","session_id":"b","user_agent":"ua","response_usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7},"request_object":{"messages":[]}}`,
	}
	if err := os.WriteFile(modelAPath, []byte(recordLines[0]+"\n"+recordLines[1]+"\n"), 0644); err != nil {
		t.Fatalf("failed to write model A archive: %v", err)
	}

	modelBPath := sessionArchiveFilePath("claude-3-7-sonnet", targetDay)
	if err := os.MkdirAll(filepath.Dir(modelBPath), 0755); err != nil {
		t.Fatalf("failed to create model directory: %v", err)
	}
	if err := os.WriteFile(modelBPath, []byte(`{"record_type":"session_turn","origin_model_name":"wrong","prompt_tokens":5,"completion_tokens":6,"total_tokens":11}`+"\n"), 0644); err != nil {
		t.Fatalf("failed to write model B archive: %v", err)
	}

	if err := writeSessionArchiveDailySummary(targetDay, generatedAt); err != nil {
		t.Fatalf("writeSessionArchiveDailySummary returned error: %v", err)
	}

	summaryPath := sessionArchiveSummaryFilePath(targetDay)
	data, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatalf("failed to read summary file: %v", err)
	}

	var summary SessionArchiveDailySummary
	if err := common.Unmarshal(data, &summary); err != nil {
		t.Fatalf("failed to unmarshal summary: %v", err)
	}
	if summary.Date != "2026-05-12" {
		t.Fatalf("unexpected summary date: %s", summary.Date)
	}
	modelA := summary.Models["gpt-4o"]
	if modelA == nil {
		t.Fatal("expected gpt-4o summary")
	}
	if modelA.InputTokens != 13 || modelA.OutputTokens != 24 || modelA.TotalTokens != 37 || modelA.SessionCount != 2 {
		t.Fatalf("unexpected gpt-4o summary: %+v", modelA)
	}
	modelB := summary.Models["claude-3-7-sonnet"]
	if modelB == nil {
		t.Fatal("expected claude-3-7-sonnet summary")
	}
	if modelB.InputTokens != 5 || modelB.OutputTokens != 6 || modelB.TotalTokens != 11 || modelB.SessionCount != 1 {
		t.Fatalf("unexpected claude-3-7-sonnet summary: %+v", modelB)
	}
	if summary.Models["wrong"] != nil {
		t.Fatalf("summary should use archive directory name, got models: %#v", summary.Models)
	}
}

func TestFinalizeSessionArchiveIgnoresPartialGinContext(t *testing.T) {
	tempDir := t.TempDir()
	originalDir := common.SessionArchiveDir
	originalEnabled := common.SessionArchiveEnabled
	common.SessionArchiveDir = tempDir
	common.SessionArchiveEnabled = true
	defer func() {
		common.SessionArchiveDir = originalDir
		common.SessionArchiveEnabled = originalEnabled
	}()

	c := &gin.Context{}
	capture := &SessionArchiveCapture{
		startedAt:        time.Date(2026, 5, 13, 12, 0, 0, 0, time.Local),
		archiveModelName: "gpt-test",
		requestObject: map[string]any{
			"messages": []any{
				map[string]any{"role": "user", "content": "ping"},
			},
		},
	}
	capture.appendHTTPResponse([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
	common.SetContextKey(c, constant.ContextKeySessionArchiveCapture, capture)

	defer func() {
		if err := recover(); err != nil {
			t.Fatalf("FinalizeSessionArchive should not panic: %v", err)
		}
	}()
	FinalizeSessionArchive(c, &relaycommon.RelayInfo{
		OriginModelName: "gpt-test",
		RequestHeaders:  map[string]string{"User-Agent": "unit-test/1.0"},
	}, nil, nil)
}

func archiveRecordForTest(sessionID string, userText string, responseText string) *SessionArchiveRecord {
	return &SessionArchiveRecord{
		RecordType:      sessionArchiveRecordType,
		SessionKey:      sessionID,
		OriginModelName: "claude-opus-4-6",
		UpstreamModel:   "claude-opus-4-6",
		UserAgent:       "unit-test/1.0",
		RequestObject: map[string]any{
			"messages": []any{
				map[string]any{"role": "user", "content": userText},
			},
		},
		ResponseText: responseText,
	}
}

func readArchiveLines(t *testing.T, root string, model string, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, model, name))
	if err != nil {
		t.Fatalf("failed to read archive: %v", err)
	}
	rawLines := strings.Split(strings.TrimSpace(string(data)), "\n")
	lines := make([]string, 0, len(rawLines))
	for _, line := range rawLines {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func readSingleArchiveLineAsMap(t *testing.T, root string, model string, name string) map[string]any {
	t.Helper()
	lines := readArchiveLines(t, root, model, name)
	if len(lines) != 1 {
		t.Fatalf("expected one archive line, got %d: %s", len(lines), strings.Join(lines, "\n"))
	}
	var saved map[string]any
	if err := common.Unmarshal([]byte(lines[0]), &saved); err != nil {
		t.Fatalf("failed to unmarshal archive line: %v", err)
	}
	return saved
}

func assertTopLevelOnlyAllowedFields(t *testing.T, saved map[string]any) {
	t.Helper()
	allowed := map[string]bool{
		"record_type":    true,
		"session_id":     true,
		"user_agent":     true,
		"response_usage": true,
		"request_object": true,
	}
	for key := range saved {
		if !allowed[key] {
			t.Fatalf("archive record contains forbidden top-level field %q: %#v", key, saved)
		}
	}
	for _, key := range []string{"record_type", "session_id", "user_agent", "request_object"} {
		if _, exists := saved[key]; !exists {
			t.Fatalf("archive record missing required top-level field %q: %#v", key, saved)
		}
	}
}

func assertRequestObjectOnlyAllowedFields(t *testing.T, requestObject map[string]any) {
	t.Helper()
	for key := range requestObject {
		if key != "messages" && key != "tools" {
			t.Fatalf("request_object contains forbidden field %q: %#v", key, requestObject)
		}
	}
	if _, exists := requestObject["messages"]; !exists {
		t.Fatalf("request_object missing messages: %#v", requestObject)
	}
}

func requestObjectMessages(t *testing.T, requestObject map[string]any) []any {
	t.Helper()
	messages, ok := requestObject["messages"].([]any)
	if !ok {
		t.Fatalf("expected request_object.messages array, got %#v", requestObject["messages"])
	}
	return messages
}

func firstContentBlock(t *testing.T, messageAny any) map[string]any {
	t.Helper()
	message, ok := messageAny.(map[string]any)
	if !ok {
		t.Fatalf("expected message map, got %#v", messageAny)
	}
	content, ok := message["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("expected message content array, got %#v", message["content"])
	}
	block, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("expected content block map, got %#v", content[0])
	}
	return block
}

func hasAnyKey(m map[string]any, keys ...string) bool {
	for _, key := range keys {
		if _, exists := m[key]; exists {
			return true
		}
	}
	return false
}

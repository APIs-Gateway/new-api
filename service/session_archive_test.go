package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
)

func TestBuildCleanSessionArchiveRecord(t *testing.T) {
	raw := &sessionArchiveRawRecord{
		RecordType:      sessionArchiveRecordType,
		RequestMethod:   "POST",
		IsStream:        true,
		OriginModelName: "gpt-test",
		UpstreamModel:   "gpt-test-upstream",
		RequestObject: map[string]any{
			"model":  "gpt-test",
			"system": "remove-me",
		},
		RequestBody:  `{"model":"gpt-test"}`,
		ResponseBody: `{"text":"hello"}`,
		ResponseText: "hello",
		RequestHeaders: map[string]string{
			"Authorization": "secret",
			"User-Agent":    "claude-cli/1.0",
		},
		ResponseUsage: &SessionArchiveUsageRecord{
			PromptTokens:     11,
			CompletionTokens: 22,
			TotalTokens:      33,
		},
		PromptTokens:     11,
		CompletionTokens: 22,
		TotalTokens:      33,
	}

	cleaned := buildCleanSessionArchiveRecord(raw)
	if cleaned == nil {
		t.Fatal("expected cleaned record")
	}
	if cleaned.RequestBody != "" {
		t.Fatalf("expected request_body to be removed when request_object exists, got %q", cleaned.RequestBody)
	}
	requestObject, ok := cleaned.RequestObject.(map[string]any)
	if !ok {
		t.Fatalf("expected request_object map, got %T", cleaned.RequestObject)
	}
	if _, exists := requestObject["system"]; exists {
		t.Fatal("expected request_object.system to be removed")
	}
	if cleaned.RequestHeaders == nil || len(cleaned.RequestHeaders) != 1 || cleaned.RequestHeaders["User-Agent"] != "claude-cli/1.0" {
		t.Fatalf("unexpected request_headers: %#v", cleaned.RequestHeaders)
	}
	if cleaned.ResponseUsage == nil {
		t.Fatal("expected response_usage")
	}
	if cleaned.ResponseUsage.PromptTokens != 11 || cleaned.ResponseUsage.CompletionTokens != 22 || cleaned.ResponseUsage.TotalTokens != 33 {
		t.Fatalf("unexpected response_usage: %#v", cleaned.ResponseUsage)
	}
	if cleaned.PromptTokens != 11 || cleaned.CompletionTokens != 22 || cleaned.TotalTokens != 33 {
		t.Fatalf("unexpected top-level token fields: %+v", cleaned)
	}
}

func TestBuildCleanSessionArchiveRecordRewritesArchiveModelFields(t *testing.T) {
	raw := &sessionArchiveRawRecord{
		RecordType:      sessionArchiveRecordType,
		RequestMethod:   "POST",
		IsStream:        true,
		OriginModelName: "claude-opus-4-6",
		UpstreamModel:   "claude-opus-4-6",
		RequestObject: map[string]any{
			"model":    "claude-opus-4-7",
			"messages": []any{map[string]any{"role": "user", "content": "use claude-opus-4-7 literally"}},
		},
		RequestBody:  `{"model":"claude-opus-4-7","messages":[{"role":"user","content":"use claude-opus-4-7 literally"}]}`,
		ResponseBody: "data: {\"model\":\"claude-opus-4-7\",\"upstream_model\":\"claude-opus-4-7\",\"downstream_model\":\"claude-opus-4-7\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n",
		ResponseText: "hello",
	}

	cleaned := buildCleanSessionArchiveRecord(raw)
	if cleaned == nil {
		t.Fatal("expected cleaned record")
	}
	if cleaned.OriginModelName != "claude-opus-4-6" || cleaned.UpstreamModel != "claude-opus-4-6" {
		t.Fatalf("unexpected model metadata: %+v", cleaned)
	}
	requestObject, ok := cleaned.RequestObject.(map[string]any)
	if !ok {
		t.Fatalf("expected request_object map, got %T", cleaned.RequestObject)
	}
	if requestObject["model"] != "claude-opus-4-6" {
		t.Fatalf("expected request_object.model to be rewritten, got %#v", requestObject["model"])
	}
	messages, ok := requestObject["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("expected request_object.messages to be preserved, got %#v", requestObject["messages"])
	}
	message, ok := messages[0].(map[string]any)
	if !ok || message["content"] != "use claude-opus-4-7 literally" {
		t.Fatalf("expected message content to be preserved, got %#v", messages[0])
	}
	if cleaned.RequestBody != "" {
		t.Fatalf("expected request_body to be removed when request_object exists, got %q", cleaned.RequestBody)
	}
	if !strings.Contains(cleaned.ResponseBody, `"model":"claude-opus-4-6"`) {
		t.Fatalf("expected response_body model rewrite, got %s", cleaned.ResponseBody)
	}
	if !strings.Contains(cleaned.ResponseBody, `"upstream_model":"claude-opus-4-6"`) {
		t.Fatalf("expected response_body upstream model rewrite, got %s", cleaned.ResponseBody)
	}
	if !strings.Contains(cleaned.ResponseBody, `"downstream_model":"claude-opus-4-6"`) {
		t.Fatalf("expected response_body downstream model rewrite, got %s", cleaned.ResponseBody)
	}
	if !strings.Contains(cleaned.ResponseBody, "data: [DONE]") {
		t.Fatalf("expected DONE marker to be preserved, got %s", cleaned.ResponseBody)
	}
	if strings.Contains(cleaned.ResponseBody, "claude-opus-4-7") {
		t.Fatalf("expected response model name to be rewritten, got %s", cleaned.ResponseBody)
	}
	if cleaned.ResponseText != "hello" {
		t.Fatalf("unexpected response_text: %q", cleaned.ResponseText)
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

func TestShouldKeepSessionArchiveRecord(t *testing.T) {
	base := &sessionArchiveRawRecord{
		TurnComplete:       true,
		ResponseHTTPStatus: 200,
		RequestHeaders: map[string]string{
			"User-Agent": "claude-cli/1.0",
		},
	}
	if !shouldKeepSessionArchiveRecord(base) {
		t.Fatal("expected successful record to be kept")
	}

	checkCX := &sessionArchiveRawRecord{
		TurnComplete:       true,
		ResponseHTTPStatus: 200,
		RequestHeaders: map[string]string{
			"User-Agent": "check-cx/1.0",
		},
	}
	if shouldKeepSessionArchiveRecord(checkCX) {
		t.Fatal("expected check-cx record to be filtered out")
	}

	failed := &sessionArchiveRawRecord{
		TurnComplete:       false,
		ResponseHTTPStatus: 200,
		RequestHeaders: map[string]string{
			"User-Agent": "claude-cli/1.0",
		},
	}
	if shouldKeepSessionArchiveRecord(failed) {
		t.Fatal("expected incomplete record to be filtered out")
	}

	non200 := &sessionArchiveRawRecord{
		TurnComplete:       true,
		ResponseHTTPStatus: 500,
		RequestHeaders: map[string]string{
			"User-Agent": "claude-cli/1.0",
		},
	}
	if shouldKeepSessionArchiveRecord(non200) {
		t.Fatal("expected non-200 record to be filtered out")
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
	record := &SessionArchiveRecord{
		RecordType:      sessionArchiveRecordType,
		OriginModelName: "claude-opus-4-6",
		UpstreamModel:   "claude-opus-4-6",
		PromptTokens:    1,
	}
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
		`{"record_type":"session_turn","origin_model_name":"gpt-4o","prompt_tokens":10,"completion_tokens":20,"total_tokens":30}`,
		`{"record_type":"session_turn","origin_model_name":"gpt-4o","response_usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`,
	}
	if err := os.WriteFile(modelAPath, []byte(recordLines[0]+"\n"+recordLines[1]+"\n"), 0644); err != nil {
		t.Fatalf("failed to write model A archive: %v", err)
	}

	modelBPath := sessionArchiveFilePath("claude-3-7-sonnet", targetDay)
	if err := os.MkdirAll(filepath.Dir(modelBPath), 0755); err != nil {
		t.Fatalf("failed to create model directory: %v", err)
	}
	if err := os.WriteFile(modelBPath, []byte(`{"record_type":"session_turn","origin_model_name":"claude-3-7-sonnet","prompt_tokens":5,"completion_tokens":6,"total_tokens":11}`+"\n"), 0644); err != nil {
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
}

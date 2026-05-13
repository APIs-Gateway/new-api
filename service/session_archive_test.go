package service

import (
	"os"
	"path/filepath"
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

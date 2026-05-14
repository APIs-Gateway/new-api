package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

const (
	sessionArchiveRecordType = "session_turn"
	sessionArchiveSubDir     = "session-jsonl"
)

var sessionArchiveWriteMu sync.Mutex

type SessionArchiveCapture struct {
	mu sync.Mutex

	startedAt time.Time

	requestModelName string
	archiveModelName string
	requestBody      string
	requestObject    any
	sessionKey       string
	sessionKeySeed   string

	httpResponse bytes.Buffer
	wsMessages   []SessionArchiveWSMessage
}

type SessionArchiveWSMessage struct {
	Kind    string `json:"kind,omitempty"`
	Payload string `json:"payload,omitempty"`
	At      int64  `json:"at,omitempty"`
}

type SessionToolDefinition struct {
	Type        string `json:"type,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
	InputSchema any    `json:"input_schema,omitempty"`
	Raw         any    `json:"raw,omitempty"`
}

type SessionToolCall struct {
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Type      string `json:"type,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Index     int    `json:"index,omitempty"`
	Raw       any    `json:"raw,omitempty"`
}

type SessionArchiveStreamStatus struct {
	EndReason  string `json:"end_reason,omitempty"`
	EndError   string `json:"end_error,omitempty"`
	ErrorCount int    `json:"error_count,omitempty"`
	NormalEnd  bool   `json:"normal_end"`
}

type SessionArchiveRecord struct {
	RecordType    string                     `json:"record_type"`
	SessionID     string                     `json:"session_id"`
	UserAgent     string                     `json:"user_agent"`
	ResponseUsage *SessionArchiveUsageRecord `json:"response_usage"`
	RequestObject any                        `json:"request_object"`

	SessionKey      string            `json:"-"`
	RequestMethod   string            `json:"-"`
	IsStream        bool              `json:"-"`
	OriginModelName string            `json:"-"`
	UpstreamModel   string            `json:"-"`
	RequestBody     string            `json:"-"`
	ResponseBody    string            `json:"-"`
	ResponseText    string            `json:"-"`
	RequestHeaders  map[string]string `json:"-"`
	sessionKeySeed  string
}

type sessionArchiveRawRecord struct {
	RecordType         string                     `json:"record_type,omitempty"`
	RequestMethod      string                     `json:"request_method,omitempty"`
	IsStream           bool                       `json:"is_stream"`
	OriginModelName    string                     `json:"origin_model_name,omitempty"`
	UpstreamModel      string                     `json:"upstream_model,omitempty"`
	RequestObject      any                        `json:"request_object,omitempty"`
	RequestBody        string                     `json:"request_body,omitempty"`
	ResponseBody       string                     `json:"response_body,omitempty"`
	ResponseText       string                     `json:"response_text,omitempty"`
	RequestHeaders     map[string]string          `json:"request_headers,omitempty"`
	UserAgent          string                     `json:"user_agent,omitempty"`
	ResponseUsage      *SessionArchiveUsageRecord `json:"response_usage,omitempty"`
	TurnComplete       bool                       `json:"turn_complete"`
	StreamComplete     bool                       `json:"stream_complete"`
	ResponseHTTPStatus int                        `json:"response_http_status,omitempty"`
	PromptTokens       int                        `json:"prompt_tokens,omitempty"`
	CompletionTokens   int                        `json:"completion_tokens,omitempty"`
	TotalTokens        int                        `json:"total_tokens,omitempty"`
}

type SessionArchiveUsageRecord struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

func (usage *SessionArchiveUsageRecord) UnmarshalJSON(data []byte) error {
	type usageJSON struct {
		InputTokens      int `json:"input_tokens"`
		OutputTokens     int `json:"output_tokens"`
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	}
	var decoded usageJSON
	if err := common.Unmarshal(data, &decoded); err != nil {
		return err
	}
	usage.InputTokens = firstNonZero(decoded.InputTokens, decoded.PromptTokens)
	usage.OutputTokens = firstNonZero(decoded.OutputTokens, decoded.CompletionTokens)
	usage.TotalTokens = decoded.TotalTokens
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	return nil
}

type sessionArchiveResponseWriter struct {
	gin.ResponseWriter
	capture *SessionArchiveCapture
}

func (w *sessionArchiveResponseWriter) Write(data []byte) (int, error) {
	if w == nil || w.ResponseWriter == nil {
		return 0, fmt.Errorf("session archive response writer is nil")
	}
	n, err := w.ResponseWriter.Write(data)
	if n > 0 && w.capture != nil {
		w.capture.appendHTTPResponse(data[:n])
	}
	return n, err
}

func (w *sessionArchiveResponseWriter) WriteString(s string) (int, error) {
	if w == nil || w.ResponseWriter == nil {
		return 0, fmt.Errorf("session archive response writer is nil")
	}
	if stringWriter, ok := w.ResponseWriter.(interface {
		WriteString(string) (int, error)
	}); ok {
		n, err := stringWriter.WriteString(s)
		if n > 0 && w.capture != nil {
			w.capture.appendHTTPResponse([]byte(s[:n]))
		}
		return n, err
	}
	n, err := w.ResponseWriter.Write([]byte(s))
	if n > 0 && w.capture != nil {
		w.capture.appendHTTPResponse([]byte(s[:n]))
	}
	return n, err
}

func StartSessionArchiveCapture(c *gin.Context, info *relaycommon.RelayInfo, request dto.Request, rawRequestBody string) *SessionArchiveCapture {
	if !common.SessionArchiveEnabled || c == nil || info == nil {
		return nil
	}
	if capture := getSessionArchiveCapture(c); capture != nil {
		return capture
	}
	if rawRequestBody == "" && request != nil {
		rawRequestBody = common.GetJsonString(request)
	}
	requestModelName := sessionArchiveRequestModelName(info, rawRequestBody)
	if !sessionArchiveShouldCaptureModel(requestModelName) {
		return nil
	}
	archiveModelName := sessionArchiveModelAlias(requestModelName)
	sessionID, _ := resolveSessionID(c, info, request, rawRequestBody)
	startedAt := info.StartTime
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	capture := &SessionArchiveCapture{
		startedAt:        startedAt,
		requestModelName: requestModelName,
		archiveModelName: archiveModelName,
		requestBody:      rawRequestBody,
		requestObject:    cloneJSONValue(request),
		sessionKey:       explicitSessionArchiveKey(info, sessionID),
		sessionKeySeed:   sessionArchiveKeySeed(info),
	}
	common.SetContextKey(c, constant.ContextKeySessionArchiveCapture, capture)
	c.Writer = &sessionArchiveResponseWriter{
		ResponseWriter: c.Writer,
		capture:        capture,
	}
	return capture
}

func AppendSessionArchiveWSMessage(c *gin.Context, kind string, payload string) {
	if !common.SessionArchiveEnabled || payload == "" {
		return
	}
	capture := getSessionArchiveCapture(c)
	if capture == nil {
		return
	}
	capture.appendWSMessage(kind, payload)
}

func FinalizeSessionArchive(c *gin.Context, info *relaycommon.RelayInfo, request dto.Request, newAPIError *types.NewAPIError) {
	if !common.SessionArchiveEnabled || c == nil || info == nil {
		return
	}
	capture := getSessionArchiveCapture(c)
	if capture == nil {
		return
	}

	responseBody := capture.snapshotResponse()
	responseUsage := slimSessionArchiveUsage(extractResponseUsage(responseBody))
	responseHTTPStatus := c.Writer.Status()
	if responseHTTPStatus == 0 && newAPIError != nil {
		responseHTTPStatus = newAPIError.StatusCode
	}
	responseText := extractResponseText(responseBody)
	rawRecord := &sessionArchiveRawRecord{
		RecordType:         sessionArchiveRecordType,
		RequestMethod:      c.Request.Method,
		IsStream:           info.IsStream,
		OriginModelName:    capture.archiveModelName,
		UpstreamModel:      capture.archiveModelName,
		RequestObject:      cloneJSONValue(capture.requestObject),
		RequestBody:        capture.requestBody,
		ResponseBody:       responseBody,
		ResponseText:       responseText,
		RequestHeaders:     cloneStringMap(info.RequestHeaders),
		UserAgent:          rawSessionArchiveUserAgent(info.RequestHeaders),
		ResponseUsage:      responseUsage,
		TurnComplete:       newAPIError == nil,
		StreamComplete:     sessionArchiveStreamComplete(info),
		ResponseHTTPStatus: responseHTTPStatus,
	}
	if responseUsage != nil {
		rawRecord.PromptTokens = responseUsage.InputTokens
		rawRecord.CompletionTokens = responseUsage.OutputTokens
		rawRecord.TotalTokens = responseUsage.TotalTokens
	}
	if !shouldKeepSessionArchiveRecord(rawRecord) {
		common.SetContextKey(c, constant.ContextKeySessionArchiveCapture, nil)
		return
	}
	record := &SessionArchiveRecord{
		RecordType:      rawRecord.RecordType,
		RequestMethod:   rawRecord.RequestMethod,
		IsStream:        rawRecord.IsStream,
		OriginModelName: rawRecord.OriginModelName,
		UpstreamModel:   rawRecord.UpstreamModel,
		RequestObject:   rawRecord.RequestObject,
		RequestBody:     rawRecord.RequestBody,
		ResponseBody:    rawRecord.ResponseBody,
		ResponseText:    rawRecord.ResponseText,
		RequestHeaders:  rawRecord.RequestHeaders,
		UserAgent:       rawRecord.UserAgent,
		ResponseUsage:   rawRecord.ResponseUsage,
		SessionKey:      capture.sessionKey,
		sessionKeySeed:  capture.sessionKeySeed,
	}
	if err := appendSessionArchiveRecord(record, capture.archiveModelName, capture.startedAt); err != nil {
		common.SysError("failed to write session archive: " + err.Error())
	}
	common.SetContextKey(c, constant.ContextKeySessionArchiveCapture, nil)
}

func (c *SessionArchiveCapture) appendHTTPResponse(data []byte) {
	if c == nil || len(data) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = c.httpResponse.Write(data)
}

func (c *SessionArchiveCapture) appendWSMessage(kind string, payload string) {
	if c == nil || payload == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wsMessages = append(c.wsMessages, SessionArchiveWSMessage{
		Kind:    kind,
		Payload: payload,
		At:      time.Now().Unix(),
	})
}

func (c *SessionArchiveCapture) snapshotResponse() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	body := c.httpResponse.String()
	if len(c.wsMessages) == 0 {
		return body
	}
	var builder strings.Builder
	if body != "" {
		builder.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			builder.WriteByte('\n')
		}
	}
	for index, message := range c.wsMessages {
		if index > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(message.Payload)
	}
	return builder.String()
}

func getSessionArchiveCapture(c *gin.Context) *SessionArchiveCapture {
	if c == nil {
		return nil
	}
	capture, ok := common.GetContextKeyType[*SessionArchiveCapture](c, constant.ContextKeySessionArchiveCapture)
	if !ok {
		return nil
	}
	return capture
}

func sessionArchiveRequestModelName(info *relaycommon.RelayInfo, rawRequestBody string) string {
	if info != nil {
		if modelName := strings.TrimSpace(info.OriginModelName); modelName != "" {
			return modelName
		}
	}
	if strings.TrimSpace(rawRequestBody) == "" {
		return ""
	}
	var body map[string]any
	if err := common.UnmarshalJsonStr(rawRequestBody, &body); err != nil {
		return ""
	}
	return firstNonEmpty(stringFromAny(body["model"]), stringFromAny(body["model_name"]))
}

func sessionArchiveShouldCaptureModel(modelName string) bool {
	common.OptionMapRWMutex.RLock()
	raw, ok := common.OptionMap[common.SessionArchiveEnabledModelsOptionKey]
	common.OptionMapRWMutex.RUnlock()
	if !ok {
		return true
	}

	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}

	var enabledModels []string
	if err := common.UnmarshalJsonStr(raw, &enabledModels); err != nil {
		common.SysError("failed to parse session archive enabled models: " + err.Error())
		return false
	}
	if len(enabledModels) == 0 {
		return false
	}

	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return false
	}
	for _, enabledModel := range enabledModels {
		if strings.TrimSpace(enabledModel) == modelName {
			return true
		}
	}
	return false
}

func sessionArchiveModelAlias(modelName string) string {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return ""
	}
	common.OptionMapRWMutex.RLock()
	raw := common.OptionMap[common.SessionArchiveModelAliasesOptionKey]
	common.OptionMapRWMutex.RUnlock()
	if strings.TrimSpace(raw) == "" {
		return modelName
	}
	var aliases map[string]string
	if err := common.UnmarshalJsonStr(raw, &aliases); err != nil {
		common.SysError("failed to parse session archive model aliases: " + err.Error())
		return modelName
	}
	alias := strings.TrimSpace(aliases[modelName])
	if alias == "" {
		return modelName
	}
	return alias
}

func appendSessionArchiveRecord(record *SessionArchiveRecord, modelName string, startedAt time.Time) error {
	if record == nil {
		return nil
	}
	sessionKey := firstNonEmpty(record.SessionID, record.SessionKey, explicitSessionArchiveKeyFromRecord(record))
	record = normalizeCleanSessionArchiveRecord(record, modelName)
	if record.SessionKey == "" {
		record.SessionKey = sessionKey
	}
	if !sessionArchiveRecordHasUsableContext(record) || !sessionArchiveRecordHasAssistantOutput(record) || strings.TrimSpace(record.UserAgent) == "" {
		return nil
	}
	path := sessionArchiveFilePath(modelName, startedAt)
	sessionArchiveWriteMu.Lock()
	defer sessionArchiveWriteMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	lines, err := readSessionArchiveLines(path)
	if err != nil {
		return err
	}
	if strings.TrimSpace(record.SessionKey) == "" {
		record.SessionKey = resolveSessionArchiveRecordKey(record, lines)
	}
	record.SessionID = record.SessionKey
	line, err := common.Marshal(record)
	if err != nil {
		return err
	}
	return writeSessionArchiveLines(path, upsertSessionArchiveLine(lines, record, string(line)))
}

func readSessionArchiveLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	rawLines := strings.Split(string(data), "\n")
	lines := make([]string, 0, len(rawLines))
	for _, line := range rawLines {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

func writeSessionArchiveLines(path string, lines []string) error {
	var builder strings.Builder
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		builder.WriteString(line)
		builder.WriteByte('\n')
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(builder.String()), 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func upsertSessionArchiveLine(lines []string, record *SessionArchiveRecord, line string) []string {
	sessionKey := ""
	if record != nil {
		sessionKey = strings.TrimSpace(firstNonEmpty(record.SessionID, record.SessionKey))
	}
	if sessionKey == "" {
		return append(lines, line)
	}
	for index, existingLine := range lines {
		var existing SessionArchiveRecord
		if err := common.Unmarshal([]byte(existingLine), &existing); err != nil {
			continue
		}
		if strings.TrimSpace(firstNonEmpty(existing.SessionID, existing.SessionKey)) == sessionKey {
			lines[index] = line
			return lines
		}
	}
	if record != nil {
		newMessages := sessionArchiveRecordMessages(record)
		for index, existingLine := range lines {
			var existing SessionArchiveRecord
			if err := common.Unmarshal([]byte(existingLine), &existing); err != nil {
				continue
			}
			normalizedExisting := normalizeCleanSessionArchiveRecord(&existing, firstNonEmpty(record.OriginModelName, record.UpstreamModel))
			existingMessages := sessionArchiveRecordMessages(normalizedExisting)
			if len(existingMessages) > 0 && sessionArchiveMessagesPrefixEqual(existingMessages, newMessages) {
				lines[index] = line
				return lines
			}
		}
	}
	return append(lines, line)
}

func resolveSessionArchiveRecordKey(record *SessionArchiveRecord, existingLines []string) string {
	if record == nil {
		return ""
	}
	if explicit := explicitSessionArchiveKeyFromRecord(record); explicit != "" {
		return explicit
	}
	if matched := matchExistingSessionArchiveKey(record, existingLines); matched != "" {
		return matched
	}
	return fallbackSessionArchiveKey(record)
}

func explicitSessionArchiveKey(info *relaycommon.RelayInfo, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || isRequestIDSessionArchiveKey(info, sessionID) {
		return ""
	}
	return "sid_" + shortSHA256(sessionID)
}

func explicitSessionArchiveKeyFromRecord(record *SessionArchiveRecord) string {
	if record == nil {
		return ""
	}
	if key := sessionKeyFromRequestObject(record.RequestObject); key != "" {
		return key
	}
	if strings.TrimSpace(record.RequestBody) == "" {
		return ""
	}
	var body map[string]any
	if err := common.UnmarshalJsonStr(record.RequestBody, &body); err != nil {
		return ""
	}
	return sessionKeyFromRequestObject(body)
}

func sessionKeyFromRequestObject(value any) string {
	requestObject, ok := mapFromAny(value)
	if !ok {
		return ""
	}
	for _, key := range []string{"session_id", "sessionId", "conversation_id", "conversationId", "prompt_cache_key"} {
		if value := stringFromAny(requestObject[key]); value != "" {
			return "sid_" + shortSHA256(value)
		}
	}
	if metadata, ok := mapFromAny(requestObject["metadata"]); ok {
		if value := metadataSessionID(metadata); value != "" {
			return "sid_" + shortSHA256(value)
		}
	}
	if conversation, ok := mapFromAny(requestObject["conversation"]); ok {
		if value := objectSessionID(conversation); value != "" {
			return "sid_" + shortSHA256(value)
		}
	}
	if value := stringFromAny(requestObject["conversation"]); value != "" {
		return "sid_" + shortSHA256(value)
	}
	return ""
}

func matchExistingSessionArchiveKey(record *SessionArchiveRecord, existingLines []string) string {
	newMessages := sessionArchiveRecordMessages(record)
	if len(newMessages) == 0 {
		return ""
	}
	for index := len(existingLines) - 1; index >= 0; index-- {
		var existing SessionArchiveRecord
		if err := common.Unmarshal([]byte(existingLines[index]), &existing); err != nil {
			continue
		}
		normalizedExisting := normalizeCleanSessionArchiveRecord(&existing, firstNonEmpty(record.OriginModelName, record.UpstreamModel))
		existingMessages := sessionArchiveRecordMessages(normalizedExisting)
		if len(existingMessages) == 0 || len(existingMessages) > len(newMessages) {
			continue
		}
		if sessionArchiveMessagesPrefixEqual(existingMessages, newMessages) {
			if key := strings.TrimSpace(firstNonEmpty(existing.SessionID, existing.SessionKey)); key != "" {
				return key
			}
			return fallbackSessionArchiveKey(normalizedExisting)
		}
	}
	return ""
}

func fallbackSessionArchiveKey(record *SessionArchiveRecord) string {
	if record == nil {
		return "ctx_" + shortSHA256(common.GetTimeString())
	}
	seed := strings.TrimSpace(record.sessionKeySeed)
	if seed == "" {
		seed = firstNonEmpty(record.OriginModelName, record.UpstreamModel)
	}
	messages := sessionArchiveRecordMessages(record)
	if len(messages) > 0 {
		messagesJSON, err := common.Marshal(messages)
		if err == nil {
			return "ctx_" + shortSHA256(seed+"|"+string(messagesJSON))
		}
	}
	if body := strings.TrimSpace(record.RequestBody); body != "" {
		return "ctx_" + shortSHA256(seed+"|"+body)
	}
	return "ctx_" + shortSHA256(seed+"|"+firstNonEmpty(record.ResponseText, record.UserAgent, common.GetTimeString()))
}

func sessionArchiveRecordMessages(record *SessionArchiveRecord) []any {
	if record == nil {
		return nil
	}
	requestObject, ok := mapFromAny(record.RequestObject)
	if !ok {
		return nil
	}
	messages, ok := sliceFromAny(requestObject["messages"])
	if !ok {
		return nil
	}
	return messages
}

func sessionArchiveMessagesPrefixEqual(prefix []any, messages []any) bool {
	if len(prefix) == 0 || len(prefix) > len(messages) {
		return false
	}
	for index := range prefix {
		prefixJSON, err := common.Marshal(prefix[index])
		if err != nil {
			return false
		}
		messageJSON, err := common.Marshal(messages[index])
		if err != nil {
			return false
		}
		if !bytes.Equal(prefixJSON, messageJSON) {
			return false
		}
	}
	return true
}

func isRequestIDSessionArchiveKey(info *relaycommon.RelayInfo, sessionID string) bool {
	if info == nil {
		return false
	}
	return strings.TrimSpace(sessionID) != "" && strings.TrimSpace(sessionID) == strings.TrimSpace(info.RequestId)
}

func sessionArchiveKeySeed(info *relaycommon.RelayInfo) string {
	if info == nil {
		return ""
	}
	return firstNonEmpty(info.OriginModelName, info.UpstreamModelName)
}

func shortSHA256(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:16])
}

func sessionArchiveStreamComplete(info *relaycommon.RelayInfo) bool {
	if info == nil || !info.IsStream {
		return true
	}
	if info.StreamStatus == nil {
		return true
	}
	return info.StreamStatus.IsNormalEnd() && !info.StreamStatus.HasErrors()
}

func shouldKeepSessionArchiveRecord(record *sessionArchiveRawRecord) bool {
	if record == nil {
		return false
	}
	userAgent := strings.ToLower(strings.TrimSpace(sessionArchiveRawFirstNonEmpty(record.UserAgent, rawSessionArchiveUserAgent(record.RequestHeaders))))
	if userAgent == "" {
		return false
	}
	if strings.HasPrefix(userAgent, "check-cx") {
		return false
	}
	if !record.TurnComplete || !record.StreamComplete || record.ResponseHTTPStatus >= 400 {
		return false
	}
	recordWithResponse := buildCleanSessionArchiveRecord(record)
	return sessionArchiveRecordHasUsableContext(recordWithResponse) && sessionArchiveRecordHasAssistantOutput(recordWithResponse)
}

func buildCleanSessionArchiveRecord(record *sessionArchiveRawRecord) *SessionArchiveRecord {
	if record == nil {
		return nil
	}
	responseUsage := slimSessionArchiveUsage(record.ResponseUsage)
	if responseUsage == nil {
		responseUsage = slimSessionArchiveUsage(&SessionArchiveUsageRecord{
			InputTokens:  record.PromptTokens,
			OutputTokens: record.CompletionTokens,
			TotalTokens:  record.TotalTokens,
		})
	}
	return normalizeCleanSessionArchiveRecord(&SessionArchiveRecord{
		RecordType:      record.RecordType,
		RequestMethod:   record.RequestMethod,
		IsStream:        record.IsStream,
		OriginModelName: record.OriginModelName,
		UpstreamModel:   record.UpstreamModel,
		RequestObject:   record.RequestObject,
		RequestBody:     record.RequestBody,
		ResponseBody:    record.ResponseBody,
		ResponseText:    record.ResponseText,
		RequestHeaders:  record.RequestHeaders,
		UserAgent:       sessionArchiveRawFirstNonEmpty(record.UserAgent, rawSessionArchiveUserAgent(record.RequestHeaders)),
		ResponseUsage:   responseUsage,
	}, record.OriginModelName)
}

func normalizeCleanSessionArchiveRecord(record *SessionArchiveRecord, modelName string) *SessionArchiveRecord {
	if record == nil {
		return nil
	}
	archiveModelName := firstNonEmpty(record.OriginModelName, modelName)
	upstreamModel := firstNonEmpty(record.UpstreamModel, archiveModelName)
	requestObject := normalizeSessionArchiveRequestObject(rewriteArchiveModelFields(cloneJSONValue(record.RequestObject), archiveModelName))
	requestObject = appendSessionArchiveAssistantOutput(requestObject, record.ResponseText, extractResponseToolCalls(record.ResponseBody))
	requestObject = ensureSessionArchiveToolDefinitions(requestObject)
	requestObject = filterSessionArchiveOrphanToolResults(requestObject)
	userAgent := sessionArchiveRawFirstNonEmpty(record.UserAgent, rawSessionArchiveUserAgent(record.RequestHeaders))
	cleaned := &SessionArchiveRecord{
		RecordType:      firstNonEmpty(record.RecordType, sessionArchiveRecordType),
		SessionKey:      strings.TrimSpace(record.SessionKey),
		SessionID:       firstNonEmpty(record.SessionID, strings.TrimSpace(record.SessionKey)),
		UserAgent:       userAgent,
		RequestMethod:   record.RequestMethod,
		IsStream:        record.IsStream,
		OriginModelName: archiveModelName,
		UpstreamModel:   upstreamModel,
		RequestObject:   requestObject,
		RequestBody:     rewriteArchiveBodyModelFields(record.RequestBody, archiveModelName),
		ResponseBody:    rewriteArchiveResponseModelFields(record.ResponseBody, archiveModelName),
		ResponseText:    record.ResponseText,
		RequestHeaders:  record.RequestHeaders,
		ResponseUsage:   record.ResponseUsage,
	}
	cleaned.sessionKeySeed = record.sessionKeySeed
	if cleaned.SessionKey == "" {
		cleaned.SessionKey = cleaned.SessionID
	}
	if cleaned.RequestObject != nil && cleaned.RequestBody != "" {
		cleaned.RequestBody = ""
	}
	return cleaned
}

func (record *SessionArchiveRecord) MarshalJSON() ([]byte, error) {
	if record == nil {
		return common.Marshal(nil)
	}
	type sessionArchiveRecordJSON struct {
		RecordType    string                     `json:"record_type"`
		SessionID     string                     `json:"session_id"`
		UserAgent     string                     `json:"user_agent"`
		ResponseUsage *SessionArchiveUsageRecord `json:"response_usage,omitempty"`
		RequestObject any                        `json:"request_object"`
	}
	return common.Marshal(sessionArchiveRecordJSON{
		RecordType:    record.RecordType,
		SessionID:     record.SessionID,
		UserAgent:     record.UserAgent,
		ResponseUsage: slimSessionArchiveUsage(record.ResponseUsage),
		RequestObject: record.RequestObject,
	})
}

func (record *SessionArchiveRecord) UnmarshalJSON(data []byte) error {
	type sessionArchiveRecordJSON struct {
		RecordType    string                     `json:"record_type"`
		SessionID     string                     `json:"session_id"`
		SessionKey    string                     `json:"session_key"`
		UserAgent     string                     `json:"user_agent"`
		RequestObject any                        `json:"request_object"`
		ResponseUsage *SessionArchiveUsageRecord `json:"response_usage"`
		RequestBody   string                     `json:"request_body"`
		ResponseText  string                     `json:"response_text"`
	}
	var decoded sessionArchiveRecordJSON
	if err := common.Unmarshal(data, &decoded); err != nil {
		return err
	}
	record.RecordType = decoded.RecordType
	record.SessionID = decoded.SessionID
	record.SessionKey = firstNonEmpty(decoded.SessionKey, decoded.SessionID)
	record.UserAgent = decoded.UserAgent
	record.RequestObject = decoded.RequestObject
	record.ResponseUsage = decoded.ResponseUsage
	record.RequestBody = decoded.RequestBody
	record.ResponseText = decoded.ResponseText
	return nil
}

func normalizeSessionArchiveRequestObject(value any) any {
	requestObject, ok := mapFromAny(value)
	if !ok {
		return map[string]any{"messages": []any{}}
	}
	cleaned := make(map[string]any, 2)

	if messages, ok := sliceFromAny(requestObject["messages"]); ok {
		if normalized := normalizeSessionArchiveMessages(messages); len(normalized) > 0 {
			cleaned["messages"] = normalized
		}
	}
	if _, hasMessages := cleaned["messages"]; !hasMessages {
		if normalized := normalizeSessionArchiveInputMessages(requestObject["input"]); len(normalized) > 0 {
			cleaned["messages"] = normalized
		}
	}
	if tools, ok := sliceFromAny(requestObject["tools"]); ok {
		if normalized := normalizeSessionArchiveTools(tools); len(normalized) > 0 {
			cleaned["tools"] = normalized
		}
	}
	if functions, ok := sliceFromAny(requestObject["functions"]); ok {
		if _, hasTools := cleaned["tools"]; !hasTools {
			if normalized := normalizeSessionArchiveTools(functions); len(normalized) > 0 {
				cleaned["tools"] = normalized
			}
		}
	}
	if _, hasMessages := cleaned["messages"]; !hasMessages {
		cleaned["messages"] = []any{}
	}
	return cleaned
}

func normalizeSessionArchiveInputMessages(input any) []any {
	switch value := input.(type) {
	case nil:
		return nil
	case string:
		if value == "" {
			return nil
		}
		return []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": value},
			},
		}}
	default:
		items, ok := sliceFromAny(value)
		if !ok {
			if text := sessionArchiveToolResultContent(value); text != "" {
				return []any{map[string]any{
					"role": "user",
					"content": []any{
						map[string]any{"type": "text", "text": text},
					},
				}}
			}
			return nil
		}
		messages := normalizeSessionArchiveMessages(items)
		if len(messages) > 0 {
			return messages
		}
		var content []any
		for _, itemAny := range items {
			if block, ok := normalizeSessionArchiveContentBlock(itemAny); ok {
				content = append(content, block)
			}
		}
		if len(content) == 0 {
			return nil
		}
		return []any{map[string]any{
			"role":    "user",
			"content": content,
		}}
	}
}

func copySessionArchiveRequestField(dst map[string]any, src map[string]any, key string) {
	copySessionArchiveRequestFieldAs(dst, src, key, key)
}

func copySessionArchiveRequestFieldAs(dst map[string]any, src map[string]any, srcKey string, dstKey string) {
	value, exists := src[srcKey]
	if !exists || value == nil {
		return
	}
	dst[dstKey] = value
}

func normalizeSessionArchiveMessages(messages []any) []any {
	if len(messages) == 0 {
		return nil
	}
	normalized := make([]any, 0, len(messages))
	for _, messageAny := range messages {
		message, ok := mapFromAny(messageAny)
		if !ok {
			continue
		}
		role := stringFromAny(message["role"])
		content := normalizeSessionArchiveContentBlocks(message["content"])

		if toolCalls := normalizeSessionArchiveToolCallBlocks(message["tool_calls"]); len(toolCalls) > 0 {
			content = append(content, toolCalls...)
		}
		if role == "tool" || message["tool_call_id"] != nil || message["tool_use_id"] != nil {
			block := normalizeSessionArchiveToolResultBlock(message)
			if block == nil {
				continue
			}
			role = "user"
			content = []any{block}
		}
		if role != "assistant" {
			role = "user"
		}
		if len(content) == 0 {
			continue
		}
		normalized = append(normalized, map[string]any{
			"role":    role,
			"content": content,
		})
	}
	return normalized
}

func normalizeSessionArchiveContentBlocks(content any) []any {
	switch value := content.(type) {
	case nil:
		return []any{}
	case string:
		if value == "" {
			return []any{}
		}
		return []any{map[string]any{
			"type": "text",
			"text": value,
		}}
	case []any:
		blocks := make([]any, 0, len(value))
		for _, item := range value {
			if block, ok := normalizeSessionArchiveContentBlock(item); ok {
				blocks = append(blocks, block)
			}
		}
		return blocks
	default:
		if block, ok := normalizeSessionArchiveContentBlock(value); ok {
			return []any{block}
		}
		return []any{}
	}
}

func normalizeSessionArchiveContentBlock(item any) (map[string]any, bool) {
	if text, ok := item.(string); ok {
		if text == "" {
			return nil, false
		}
		return map[string]any{"type": "text", "text": text}, true
	}
	block, ok := mapFromAny(item)
	if !ok {
		if text := rawJSONString(item); text != "" {
			return map[string]any{"type": "text", "text": text}, true
		}
		return nil, false
	}

	blockType := firstNonEmpty(stringFromAny(block["type"]), inferSessionArchiveContentBlockType(block))
	switch blockType {
	case "input_text", "output_text":
		blockType = "text"
	case "reasoning", "reasoning_content", "thinking", "image", "document":
		return nil, false
	case "function_call":
		normalized := normalizeSessionArchiveFunctionCallBlock(block)
		return normalized, normalized != nil
	case "function_call_output":
		normalized := normalizeSessionArchiveFunctionCallOutputBlock(block)
		return normalized, normalized != nil
	}

	normalized := map[string]any{"type": blockType}
	switch blockType {
	case "text":
		text := firstNonEmpty(stringFromAny(block["text"]), stringFromAny(block["content"]))
		if text == "" {
			return nil, false
		}
		normalized["text"] = text
	case "tool_use":
		id := firstNonEmpty(stringFromAny(block["id"]), stringFromAny(block["call_id"]))
		name := stringFromAny(block["name"])
		input := sessionArchiveToolInput(block["input"])
		if id == "" || name == "" {
			return nil, false
		}
		normalized["id"] = id
		normalized["name"] = name
		normalized["input"] = ensureSessionArchiveObject(input)
	case "tool_result":
		id := firstNonEmpty(stringFromAny(block["tool_use_id"]), stringFromAny(block["tool_call_id"]), stringFromAny(block["call_id"]), stringFromAny(block["id"]))
		if id == "" {
			return nil, false
		}
		normalized["tool_use_id"] = id
		normalized["content"] = sessionArchiveToolResultContent(block["content"])
		isError, ok := boolFromAny(block["is_error"])
		if !ok {
			isError = false
		}
		normalized["is_error"] = isError
	default:
		return nil, false
	}
	return normalized, true
}

func inferSessionArchiveContentBlockType(block map[string]any) string {
	switch {
	case block["tool_use_id"] != nil || block["tool_call_id"] != nil:
		return "tool_result"
	case block["id"] != nil && block["name"] != nil && block["input"] != nil:
		return "tool_use"
	case block["text"] != nil || block["content"] != nil:
		return "text"
	default:
		return ""
	}
}

func normalizeSessionArchiveToolCallBlocks(value any) []any {
	toolCalls, ok := sliceFromAny(value)
	if !ok {
		return nil
	}
	blocks := make([]any, 0, len(toolCalls))
	for _, toolCallAny := range toolCalls {
		toolCall, ok := mapFromAny(toolCallAny)
		if !ok {
			continue
		}
		if block := normalizeSessionArchiveFunctionCallBlock(toolCall); block != nil {
			blocks = append(blocks, block)
		}
	}
	return blocks
}

func normalizeSessionArchiveFunctionCallBlock(block map[string]any) map[string]any {
	function, _ := mapFromAny(block["function"])
	normalized := map[string]any{"type": "tool_use"}
	id := firstNonEmpty(stringFromAny(block["id"]), stringFromAny(block["call_id"]))
	if id == "" {
		id = "toolu_" + shortSHA256(rawJSONString(block))
	}
	name := firstNonEmpty(stringFromAny(block["name"]), stringFromAny(function["name"]))
	if name == "" {
		return nil
	}
	normalized["id"] = id
	normalized["name"] = name
	arguments := firstNonNil(function["arguments"], block["arguments"], block["input"])
	normalized["input"] = ensureSessionArchiveObject(sessionArchiveToolInput(arguments))
	return normalized
}

func normalizeSessionArchiveFunctionCallOutputBlock(block map[string]any) map[string]any {
	normalized := map[string]any{"type": "tool_result"}
	id := firstNonEmpty(stringFromAny(block["call_id"]), stringFromAny(block["tool_call_id"]), stringFromAny(block["id"]))
	if id == "" {
		return nil
	}
	normalized["tool_use_id"] = id
	normalized["content"] = sessionArchiveToolResultContent(firstNonNil(block["output"], block["content"]))
	isError, ok := boolFromAny(block["is_error"])
	if !ok {
		isError = false
	}
	normalized["is_error"] = isError
	return normalized
}

func normalizeSessionArchiveToolResultBlock(message map[string]any) map[string]any {
	normalized := map[string]any{"type": "tool_result"}
	id := firstNonEmpty(stringFromAny(message["tool_use_id"]), stringFromAny(message["tool_call_id"]), stringFromAny(message["call_id"]), stringFromAny(message["id"]))
	if id == "" {
		return nil
	}
	normalized["tool_use_id"] = id
	normalized["content"] = sessionArchiveToolResultContent(message["content"])
	isError, ok := boolFromAny(message["is_error"])
	if !ok {
		isError = false
	}
	normalized["is_error"] = isError
	return normalized
}

func normalizeSessionArchiveTools(tools []any) []any {
	if len(tools) == 0 {
		return nil
	}
	normalized := make([]any, 0, len(tools))
	for _, toolAny := range tools {
		tool, ok := mapFromAny(toolAny)
		if !ok {
			continue
		}
		function, _ := mapFromAny(tool["function"])
		item := map[string]any{}
		if name := firstNonEmpty(stringFromAny(tool["name"]), stringFromAny(function["name"])); name != "" {
			item["name"] = name
		}
		if description := firstNonEmpty(stringFromAny(tool["description"]), stringFromAny(function["description"])); description != "" {
			item["description"] = description
		}
		if inputSchema := firstNonNil(tool["input_schema"], tool["inputSchema"], tool["parameters"], function["parameters"]); inputSchema != nil {
			item["input_schema"] = inputSchema
		}
		if item["name"] != nil {
			normalized = append(normalized, item)
		}
	}
	return normalized
}

func sessionArchiveToolInput(value any) any {
	switch typed := value.(type) {
	case nil:
		return nil
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return nil
		}
		var parsed any
		if err := common.UnmarshalJsonStr(trimmed, &parsed); err == nil {
			return parsed
		}
		return map[string]any{"arguments": typed}
	case []byte:
		return sessionArchiveToolInput(string(typed))
	default:
		if _, ok := mapFromAny(value); ok {
			return value
		}
		return map[string]any{"value": value}
	}
}

func ensureSessionArchiveObject(value any) map[string]any {
	if object, ok := mapFromAny(value); ok {
		return object
	}
	if value == nil {
		return map[string]any{}
	}
	return map[string]any{"value": value}
}

func sessionArchiveToolResultContent(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []byte:
		return string(typed)
	default:
		if text := contentText(value); text != "" {
			return text
		}
		return rawJSONString(value)
	}
}

func appendSessionArchiveAssistantOutput(requestObject any, responseText string, toolCalls []SessionToolCall) any {
	object, ok := mapFromAny(requestObject)
	if !ok {
		object = map[string]any{}
	}
	messages, _ := sliceFromAny(object["messages"])
	content := make([]any, 0, 1+len(toolCalls))
	if strings.TrimSpace(responseText) != "" {
		content = append(content, map[string]any{
			"type": "text",
			"text": responseText,
		})
	}
	for _, call := range toolCalls {
		block := sessionArchiveToolCallContentBlock(call)
		if block != nil {
			content = append(content, block)
		}
	}
	if len(content) > 0 {
		messages = append(messages, map[string]any{
			"role":    "assistant",
			"content": content,
		})
	}
	object["messages"] = messages
	return object
}

func sessionArchiveToolCallContentBlock(call SessionToolCall) map[string]any {
	id := firstNonEmpty(call.CallID, call.ID)
	if id == "" {
		id = "toolu_" + shortSHA256(firstNonEmpty(call.Name, call.Arguments, rawJSONString(call.Raw)))
	}
	name := strings.TrimSpace(call.Name)
	if name == "" {
		return nil
	}
	return map[string]any{
		"type":  "tool_use",
		"id":    id,
		"name":  name,
		"input": ensureSessionArchiveObject(sessionArchiveToolInput(call.Arguments)),
	}
}

func ensureSessionArchiveToolDefinitions(requestObject any) any {
	object, ok := mapFromAny(requestObject)
	if !ok {
		return requestObject
	}
	toolNames := sessionArchiveToolUseNames(object["messages"])
	if len(toolNames) == 0 {
		return object
	}
	existing := make(map[string]bool, len(toolNames))
	tools, _ := sliceFromAny(object["tools"])
	for _, toolAny := range tools {
		tool, ok := mapFromAny(toolAny)
		if !ok {
			continue
		}
		if name := stringFromAny(tool["name"]); name != "" {
			existing[name] = true
		}
	}
	for _, name := range toolNames {
		if existing[name] {
			continue
		}
		tools = append(tools, map[string]any{"name": name})
		existing[name] = true
	}
	if len(tools) > 0 {
		object["tools"] = tools
	}
	return object
}

func filterSessionArchiveOrphanToolResults(requestObject any) any {
	object, ok := mapFromAny(requestObject)
	if !ok {
		return requestObject
	}
	toolUseIDs := sessionArchiveToolUseIDs(object["messages"])
	if len(toolUseIDs) == 0 {
		return object
	}
	messages, ok := sliceFromAny(object["messages"])
	if !ok {
		return object
	}
	filteredMessages := make([]any, 0, len(messages))
	for _, messageAny := range messages {
		message, ok := mapFromAny(messageAny)
		if !ok {
			continue
		}
		content, ok := sliceFromAny(message["content"])
		if !ok {
			filteredMessages = append(filteredMessages, message)
			continue
		}
		filteredContent := make([]any, 0, len(content))
		for _, blockAny := range content {
			block, ok := mapFromAny(blockAny)
			if !ok || stringFromAny(block["type"]) != "tool_result" {
				filteredContent = append(filteredContent, blockAny)
				continue
			}
			if toolUseIDs[stringFromAny(block["tool_use_id"])] {
				filteredContent = append(filteredContent, block)
			}
		}
		if len(filteredContent) == 0 {
			continue
		}
		message["content"] = filteredContent
		filteredMessages = append(filteredMessages, message)
	}
	object["messages"] = filteredMessages
	return object
}

func sessionArchiveToolUseIDs(messagesAny any) map[string]bool {
	messages, ok := sliceFromAny(messagesAny)
	if !ok {
		return nil
	}
	ids := make(map[string]bool)
	for _, messageAny := range messages {
		message, ok := mapFromAny(messageAny)
		if !ok || stringFromAny(message["role"]) != "assistant" {
			continue
		}
		content, ok := sliceFromAny(message["content"])
		if !ok {
			continue
		}
		for _, blockAny := range content {
			block, ok := mapFromAny(blockAny)
			if !ok || stringFromAny(block["type"]) != "tool_use" {
				continue
			}
			if id := stringFromAny(block["id"]); id != "" {
				ids[id] = true
			}
		}
	}
	return ids
}

func sessionArchiveToolUseNames(messagesAny any) []string {
	messages, ok := sliceFromAny(messagesAny)
	if !ok {
		return nil
	}
	names := make([]string, 0)
	seen := make(map[string]bool)
	for _, messageAny := range messages {
		message, ok := mapFromAny(messageAny)
		if !ok || stringFromAny(message["role"]) != "assistant" {
			continue
		}
		content, ok := sliceFromAny(message["content"])
		if !ok {
			continue
		}
		for _, blockAny := range content {
			block, ok := mapFromAny(blockAny)
			if !ok || stringFromAny(block["type"]) != "tool_use" {
				continue
			}
			name := stringFromAny(block["name"])
			if name == "" || seen[name] {
				continue
			}
			names = append(names, name)
			seen[name] = true
		}
	}
	return names
}

func sessionArchiveRecordHasUsableContext(record *SessionArchiveRecord) bool {
	if record == nil {
		return false
	}
	requestObject, ok := mapFromAny(record.RequestObject)
	if !ok {
		return false
	}
	tools, ok := sliceFromAny(requestObject["tools"])
	if ok && len(tools) > 0 {
		return true
	}
	for _, messageAny := range sessionArchiveRecordMessages(record) {
		message, ok := mapFromAny(messageAny)
		if !ok {
			continue
		}
		role := stringFromAny(message["role"])
		content, ok := sliceFromAny(message["content"])
		if !ok || len(content) == 0 {
			continue
		}
		if role == "user" {
			return true
		}
		for _, blockAny := range content {
			block, ok := mapFromAny(blockAny)
			if !ok {
				continue
			}
			switch stringFromAny(block["type"]) {
			case "tool_use", "tool_result":
				return true
			}
		}
	}
	return false
}

func sessionArchiveRecordHasAssistantOutput(record *SessionArchiveRecord) bool {
	if record == nil {
		return false
	}
	for _, messageAny := range sessionArchiveRecordMessages(record) {
		message, ok := mapFromAny(messageAny)
		if !ok || stringFromAny(message["role"]) != "assistant" {
			continue
		}
		content, ok := sliceFromAny(message["content"])
		if !ok {
			continue
		}
		for _, blockAny := range content {
			block, ok := mapFromAny(blockAny)
			if !ok {
				continue
			}
			switch stringFromAny(block["type"]) {
			case "text":
				if stringFromAny(block["text"]) != "" {
					return true
				}
			case "tool_use":
				if stringFromAny(block["id"]) != "" && stringFromAny(block["name"]) != "" {
					return true
				}
			}
		}
	}
	return false
}

func rewriteArchiveModelFields(value any, archiveModelName string) any {
	archiveModelName = strings.TrimSpace(archiveModelName)
	if value == nil || archiveModelName == "" {
		return value
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if isArchiveModelFieldKey(key) {
				typed[key] = archiveModelName
				continue
			}
			typed[key] = rewriteArchiveModelFields(item, archiveModelName)
		}
		return typed
	case []any:
		for index, item := range typed {
			typed[index] = rewriteArchiveModelFields(item, archiveModelName)
		}
		return typed
	default:
		return value
	}
}

func isArchiveModelFieldKey(key string) bool {
	switch key {
	case "model", "model_name", "origin_model_name", "original_model", "upstream_model", "downstream_model":
		return true
	default:
		return false
	}
}

func rewriteArchiveBodyModelFields(body string, archiveModelName string) string {
	archiveModelName = strings.TrimSpace(archiveModelName)
	if strings.TrimSpace(body) == "" || archiveModelName == "" {
		return body
	}
	var payload any
	if err := common.UnmarshalJsonStr(body, &payload); err != nil {
		return body
	}
	payload = rewriteArchiveModelFields(payload, archiveModelName)
	rewritten, err := common.Marshal(payload)
	if err != nil {
		return body
	}
	return string(rewritten)
}

func rewriteArchiveResponseModelFields(body string, archiveModelName string) string {
	archiveModelName = strings.TrimSpace(archiveModelName)
	if strings.TrimSpace(body) == "" || archiveModelName == "" {
		return body
	}
	if hasSSEDataLine(body) {
		return rewriteArchiveSSEModelFields(body, archiveModelName)
	}
	rewritten := rewriteArchiveBodyModelFields(body, archiveModelName)
	if rewritten != body {
		return rewritten
	}
	if strings.Contains(strings.TrimSpace(body), "\n") {
		return rewriteArchiveJSONLinesModelFields(body, archiveModelName)
	}
	return body
}

func hasSSEDataLine(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "data:") {
			return true
		}
	}
	return false
}

func rewriteArchiveSSEModelFields(body string, archiveModelName string) string {
	lines := strings.Split(body, "\n")
	rewrote := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		prefixLen := strings.Index(line, "data:")
		if prefixLen < 0 {
			continue
		}
		prefix := line[:prefixLen+len("data:")]
		payload := strings.TrimSpace(line[prefixLen+len("data:"):])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		rewritten := rewriteArchiveBodyModelFields(payload, archiveModelName)
		if rewritten == payload {
			continue
		}
		lines[i] = prefix + " " + rewritten
		rewrote = true
	}
	if !rewrote {
		return body
	}
	return strings.Join(lines, "\n")
}

func rewriteArchiveJSONLinesModelFields(body string, archiveModelName string) string {
	lines := strings.Split(body, "\n")
	rewrote := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || (!strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[")) {
			continue
		}
		rewritten := rewriteArchiveBodyModelFields(trimmed, archiveModelName)
		if rewritten == trimmed {
			continue
		}
		leading := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		lines[i] = leading + rewritten
		rewrote = true
	}
	if !rewrote {
		return body
	}
	return strings.Join(lines, "\n")
}

func slimSessionRequestHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	for key, value := range headers {
		if strings.EqualFold(strings.TrimSpace(key), "user-agent") {
			return map[string]string{"User-Agent": value}
		}
	}
	return nil
}

func rawSessionArchiveUserAgent(headers map[string]string) string {
	if len(headers) == 0 {
		return ""
	}
	for key, value := range headers {
		if strings.EqualFold(strings.TrimSpace(key), "user-agent") {
			return value
		}
	}
	return ""
}

func sessionArchiveUserAgent(headers map[string]string) string {
	if len(headers) == 0 {
		return ""
	}
	for key, value := range headers {
		if strings.EqualFold(strings.TrimSpace(key), "user-agent") {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func slimSessionArchiveUsage(usage any) *SessionArchiveUsageRecord {
	switch typed := usage.(type) {
	case nil:
		return nil
	case *SessionArchiveUsageRecord:
		if typed == nil || (typed.InputTokens == 0 && typed.OutputTokens == 0 && typed.TotalTokens == 0) {
			return nil
		}
		return &SessionArchiveUsageRecord{
			InputTokens:  typed.InputTokens,
			OutputTokens: typed.OutputTokens,
			TotalTokens:  typed.TotalTokens,
		}
	case *dto.Usage:
		if typed == nil || (typed.PromptTokens == 0 && typed.CompletionTokens == 0 && typed.TotalTokens == 0) {
			return nil
		}
		return &SessionArchiveUsageRecord{
			InputTokens:  typed.PromptTokens,
			OutputTokens: typed.CompletionTokens,
			TotalTokens:  typed.TotalTokens,
		}
	default:
		usageMap, ok := mapFromAny(typed)
		if !ok {
			return nil
		}
		slimmed := &SessionArchiveUsageRecord{
			InputTokens:  intFromAny(firstNonNil(usageMap["input_tokens"], usageMap["prompt_tokens"])),
			OutputTokens: intFromAny(firstNonNil(usageMap["output_tokens"], usageMap["completion_tokens"])),
			TotalTokens:  intFromAny(usageMap["total_tokens"]),
		}
		if slimmed.TotalTokens == 0 {
			slimmed.TotalTokens = slimmed.InputTokens + slimmed.OutputTokens
		}
		if slimmed.InputTokens == 0 && slimmed.OutputTokens == 0 && slimmed.TotalTokens == 0 {
			return nil
		}
		return slimmed
	}
}

func cloneStringMap(value map[string]string) map[string]string {
	if len(value) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(value))
	for key, item := range value {
		cloned[key] = item
	}
	return cloned
}

func sessionArchiveFilePath(modelName string, t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return filepath.Join(sessionArchiveRootDir(), sessionArchiveModelDir(modelName), fmt.Sprintf("session-%s.jsonl", t.Format("20060102")))
}

func sessionArchiveRootDir() string {
	dir := strings.TrimSpace(common.SessionArchiveDir)
	if dir != "" {
		return dir
	}
	logDir := "./logs"
	if common.LogDir != nil && strings.TrimSpace(*common.LogDir) != "" {
		logDir = *common.LogDir
	}
	return filepath.Join(logDir, sessionArchiveSubDir)
}

func sessionArchiveModelDir(modelName string) string {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return "unknown-model"
	}
	return url.PathEscape(modelName)
}

func cloneRelayFormats(in []types.RelayFormat) []types.RelayFormat {
	if len(in) == 0 {
		return nil
	}
	out := make([]types.RelayFormat, len(in))
	copy(out, in)
	return out
}

func sanitizeSessionHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(headers))
	for key, value := range headers {
		lower := strings.ToLower(strings.TrimSpace(key))
		if lower == "" {
			continue
		}
		if isSensitiveHeader(lower) {
			out[key] = "***redacted***"
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func isSensitiveHeader(key string) bool {
	if key == "authorization" || key == "proxy-authorization" || key == "cookie" || key == "set-cookie" {
		return true
	}
	return strings.Contains(key, "api-key") ||
		strings.Contains(key, "apikey") ||
		strings.Contains(key, "access-token") ||
		strings.Contains(key, "token")
}

func resolveSessionID(c *gin.Context, info *relaycommon.RelayInfo, request dto.Request, rawRequestBody string) (string, string) {
	for _, header := range []string{"X-Session-Id", "X-Session-ID", "Session-Id", "Session-ID", "Session_id", "SessionID", "session_id", "sessionId", "sessionid", "X-Conversation-Id", "Conversation-Id"} {
		if c != nil {
			if value := strings.TrimSpace(c.Request.Header.Get(header)); value != "" {
				return value, "header:" + header
			}
		}
	}
	if id, source := sessionIDFromRequest(request); id != "" {
		return id, source
	}
	if id, source := sessionIDFromRawBody(rawRequestBody); id != "" {
		return id, source
	}
	return "", ""
}

func sessionIDFromRequest(request dto.Request) (string, string) {
	switch req := request.(type) {
	case *dto.GeneralOpenAIRequest:
		if value := strings.TrimSpace(req.PromptCacheKey); value != "" {
			return value, "json:prompt_cache_key"
		}
		if value := sessionIDFromMetadata(req.Metadata); value != "" {
			return value, "json:metadata.session_id"
		}
	case *dto.OpenAIResponsesRequest:
		if value := sessionIDFromRawMessage(req.PromptCacheKey); value != "" {
			return value, "json:prompt_cache_key"
		}
		if value := sessionIDFromRawMessage(req.Conversation); value != "" {
			return value, "json:conversation"
		}
		if value := sessionIDFromMetadata(req.Metadata); value != "" {
			return value, "json:metadata.session_id"
		}
	case *dto.OpenAIResponsesCompactionRequest:
		if value := strings.TrimSpace(req.PreviousResponseID); value != "" {
			return value, "json:previous_response_id"
		}
	case *dto.ClaudeRequest:
		if value := sessionIDFromMetadata(req.Metadata); value != "" {
			return value, "json:metadata.session_id"
		}
	}
	return "", ""
}

func sessionLinkFields(request dto.Request, rawRequestBody string) (string, string) {
	var promptCacheKey string
	var previousResponseID string
	switch req := request.(type) {
	case *dto.GeneralOpenAIRequest:
		promptCacheKey = strings.TrimSpace(req.PromptCacheKey)
	case *dto.OpenAIResponsesRequest:
		promptCacheKey = sessionIDFromRawMessage(req.PromptCacheKey)
		previousResponseID = strings.TrimSpace(req.PreviousResponseID)
	case *dto.OpenAIResponsesCompactionRequest:
		previousResponseID = strings.TrimSpace(req.PreviousResponseID)
	}
	if strings.TrimSpace(rawRequestBody) == "" {
		return promptCacheKey, previousResponseID
	}
	var body map[string]any
	if err := common.UnmarshalJsonStr(rawRequestBody, &body); err != nil {
		return promptCacheKey, previousResponseID
	}
	if promptCacheKey == "" {
		promptCacheKey = stringFromAny(body["prompt_cache_key"])
	}
	if previousResponseID == "" {
		previousResponseID = stringFromAny(body["previous_response_id"])
	}
	return promptCacheKey, previousResponseID
}

func sessionIDFromRawBody(raw string) (string, string) {
	if strings.TrimSpace(raw) == "" {
		return "", ""
	}
	var body map[string]any
	if err := common.UnmarshalJsonStr(raw, &body); err != nil {
		return "", ""
	}
	for _, key := range []string{"session_id", "sessionId", "conversation", "conversation_id", "conversationId", "prompt_cache_key"} {
		switch value := body[key].(type) {
		case string:
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed, "json:" + key
			}
		case []byte:
			if trimmed := strings.TrimSpace(string(value)); trimmed != "" {
				return trimmed, "json:" + key
			}
		case map[string]any:
			if nested := objectSessionID(value); nested != "" {
				return nested, "json:" + key
			}
		}
	}
	if metaMap, ok := mapFromAny(body["metadata"]); ok {
		if value := metadataSessionID(metaMap); value != "" {
			return value, "json:metadata.session_id"
		}
	}
	return "", ""
}

func sessionIDFromMetadata(metadata []byte) string {
	if len(metadata) == 0 {
		return ""
	}
	var m map[string]any
	if err := common.Unmarshal(metadata, &m); err != nil {
		return ""
	}
	return metadataSessionID(m)
}

func sessionIDFromRawMessage(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	switch common.GetJsonType(raw) {
	case "string", "number", "boolean":
		return strings.TrimSpace(common.JsonRawMessageToString(raw))
	case "object", "array":
		var m map[string]any
		if err := common.Unmarshal(raw, &m); err == nil {
			return objectSessionID(m)
		}
	}
	return ""
}

func objectSessionID(data map[string]any) string {
	if len(data) == 0 {
		return ""
	}
	if value := metadataSessionID(data); value != "" {
		return value
	}
	if value := stringFromAny(data["id"]); value != "" {
		return value
	}
	return ""
}

func metadataSessionID(metadata map[string]any) string {
	for _, key := range []string{"session_id", "sessionId", "conversation_id", "conversationId"} {
		if value := stringFromAny(metadata[key]); value != "" {
			return value
		}
	}
	return ""
}

func extractToolDefinitions(request dto.Request) []SessionToolDefinition {
	switch req := request.(type) {
	case *dto.GeneralOpenAIRequest:
		defs := make([]SessionToolDefinition, 0, len(req.Tools))
		for _, tool := range req.Tools {
			defs = append(defs, SessionToolDefinition{
				Type:        stringFromAny(tool.Type),
				Name:        tool.Function.Name,
				Description: tool.Function.Description,
				Parameters:  tool.Function.Parameters,
				Raw:         tool,
			})
		}
		if len(req.Functions) > 0 {
			defs = append(defs, extractToolDefinitionsFromRaw(req.Functions)...)
		}
		return defs
	case *dto.OpenAIResponsesRequest:
		return extractToolDefinitionsFromRaw(req.Tools)
	case *dto.ClaudeRequest:
		return extractToolDefinitionsFromAny(req.Tools)
	default:
		return nil
	}
}

func extractToolDefinitionsFromRaw(raw []byte) []SessionToolDefinition {
	if len(raw) == 0 {
		return nil
	}
	var tools []map[string]any
	if err := common.Unmarshal(raw, &tools); err != nil {
		return nil
	}
	return toolDefinitionsFromMaps(tools)
}

func extractToolDefinitionsFromAny(tools any) []SessionToolDefinition {
	if tools == nil {
		return nil
	}
	if toolMaps, err := common.Any2Type[[]map[string]any](tools); err == nil {
		return toolDefinitionsFromMaps(toolMaps)
	}
	return nil
}

func toolDefinitionsFromMaps(tools []map[string]any) []SessionToolDefinition {
	if len(tools) == 0 {
		return nil
	}
	defs := make([]SessionToolDefinition, 0, len(tools))
	for _, tool := range tools {
		def := SessionToolDefinition{
			Type:        stringFromAny(tool["type"]),
			Name:        stringFromAny(tool["name"]),
			Description: stringFromAny(tool["description"]),
			Parameters:  tool["parameters"],
			InputSchema: tool["input_schema"],
			Raw:         tool,
		}
		if fn, ok := mapFromAny(tool["function"]); ok {
			def.Type = firstNonEmpty(def.Type, "function")
			def.Name = firstNonEmpty(def.Name, stringFromAny(fn["name"]))
			def.Description = firstNonEmpty(def.Description, stringFromAny(fn["description"]))
			if def.Parameters == nil {
				def.Parameters = fn["parameters"]
			}
		}
		if schema := tool["inputSchema"]; schema != nil && def.InputSchema == nil {
			def.InputSchema = schema
		}
		if def.Type == "" && (def.Name != "" || def.Parameters != nil || def.InputSchema != nil) {
			def.Type = "function"
		}
		defs = append(defs, def)
	}
	return defs
}

func extractResponseText(body string) string {
	if strings.TrimSpace(body) == "" {
		return ""
	}
	var builder strings.Builder
	for _, fragment := range responseJSONFragments(body) {
		var payload any
		if err := common.UnmarshalJsonStr(fragment, &payload); err != nil {
			continue
		}
		builder.WriteString(textFromPayload(payload))
	}
	return builder.String()
}

func extractResponseToolCalls(body string) []SessionToolCall {
	fragments := responseJSONFragments(body)
	if len(fragments) == 0 {
		return nil
	}
	callsByKey := make(map[string]*SessionToolCall)
	keys := make([]string, 0)
	for _, fragment := range fragments {
		var payload any
		if err := common.UnmarshalJsonStr(fragment, &payload); err != nil {
			continue
		}
		extractToolCallsFromPayload(payload, callsByKey, &keys)
	}
	if len(keys) == 0 {
		return nil
	}
	result := make([]SessionToolCall, 0, len(keys))
	for _, key := range keys {
		if call := callsByKey[key]; call != nil {
			result = append(result, *call)
		}
	}
	return result
}

func extractResponseUsage(body string) *dto.Usage {
	var last *dto.Usage
	for _, fragment := range responseJSONFragments(body) {
		var payload any
		if err := common.UnmarshalJsonStr(fragment, &payload); err != nil {
			continue
		}
		if usage := usageFromPayload(payload); usage != nil {
			last = usage
		}
	}
	return last
}

func extractResponseID(body string) string {
	for _, fragment := range responseJSONFragments(body) {
		var payload any
		if err := common.UnmarshalJsonStr(fragment, &payload); err != nil {
			continue
		}
		if id := responseIDFromPayload(payload); id != "" {
			return id
		}
	}
	return ""
}

func responseIDFromPayload(payload any) string {
	switch value := payload.(type) {
	case []any:
		for _, item := range value {
			if id := responseIDFromPayload(item); id != "" {
				return id
			}
		}
	case map[string]any:
		if id := stringFromAny(value["id"]); id != "" {
			return id
		}
		if response, ok := mapFromAny(value["response"]); ok {
			if id := stringFromAny(response["id"]); id != "" {
				return id
			}
		}
	}
	return ""
}

func responseJSONFragments(body string) []string {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return nil
	}
	if strings.Contains(trimmed, "data:") {
		lines := strings.Split(trimmed, "\n")
		fragments := make([]string, 0, len(lines))
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" || payload == "[DONE]" {
				continue
			}
			fragments = append(fragments, payload)
		}
		if len(fragments) > 0 {
			return fragments
		}
	}
	if strings.Contains(trimmed, "\n") {
		lines := strings.Split(trimmed, "\n")
		fragments := make([]string, 0, len(lines))
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "{") || strings.HasPrefix(line, "[") {
				fragments = append(fragments, line)
			}
		}
		if len(fragments) > 0 {
			return fragments
		}
	}
	return []string{trimmed}
}

func textFromPayload(payload any) string {
	switch value := payload.(type) {
	case []any:
		var builder strings.Builder
		for _, item := range value {
			builder.WriteString(textFromPayload(item))
		}
		return builder.String()
	case map[string]any:
		var builder strings.Builder
		if choices, ok := sliceFromAny(value["choices"]); ok {
			for _, choiceAny := range choices {
				choice, ok := mapFromAny(choiceAny)
				if !ok {
					continue
				}
				if text := stringFromAny(choice["text"]); text != "" {
					builder.WriteString(text)
				}
				if msg, ok := mapFromAny(choice["message"]); ok {
					builder.WriteString(contentText(msg["content"]))
				}
				if delta, ok := mapFromAny(choice["delta"]); ok {
					builder.WriteString(contentText(delta["content"]))
				}
			}
		}
		if outputs, ok := sliceFromAny(value["output"]); ok {
			for _, outputAny := range outputs {
				output, ok := mapFromAny(outputAny)
				if !ok {
					continue
				}
				builder.WriteString(contentText(output["content"]))
				builder.WriteString(stringFromAny(output["text"]))
			}
		}
		if stringFromAny(value["type"]) == "output_text" {
			builder.WriteString(stringFromAny(value["text"]))
		}
		builder.WriteString(contentText(value["content"]))
		if delta, ok := mapFromAny(value["delta"]); ok {
			builder.WriteString(contentText(delta["text"]))
		}
		builder.WriteString(stringFromAny(value["text"]))
		return builder.String()
	case string:
		return value
	default:
		return ""
	}
}

func contentText(value any) string {
	switch content := value.(type) {
	case nil:
		return ""
	case string:
		return content
	case []any:
		var builder strings.Builder
		for _, item := range content {
			itemMap, ok := mapFromAny(item)
			if !ok {
				builder.WriteString(contentText(item))
				continue
			}
			builder.WriteString(stringFromAny(itemMap["text"]))
		}
		return builder.String()
	default:
		return rawJSONString(content)
	}
}

func extractToolCallsFromPayload(payload any, callsByKey map[string]*SessionToolCall, keys *[]string) {
	switch value := payload.(type) {
	case []any:
		for _, item := range value {
			extractToolCallsFromPayload(item, callsByKey, keys)
		}
	case map[string]any:
		if choices, ok := sliceFromAny(value["choices"]); ok {
			for _, choiceAny := range choices {
				choice, ok := mapFromAny(choiceAny)
				if !ok {
					continue
				}
				if msg, ok := mapFromAny(choice["message"]); ok {
					mergeToolCalls(msg["tool_calls"], callsByKey, keys)
				}
				if delta, ok := mapFromAny(choice["delta"]); ok {
					mergeToolCalls(delta["tool_calls"], callsByKey, keys)
				}
			}
		}
		if outputs, ok := sliceFromAny(value["output"]); ok {
			for _, outputAny := range outputs {
				output, ok := mapFromAny(outputAny)
				if !ok || stringFromAny(output["type"]) != "function_call" {
					continue
				}
				call := SessionToolCall{
					ID:        stringFromAny(output["id"]),
					CallID:    stringFromAny(output["call_id"]),
					Type:      "function",
					Name:      stringFromAny(output["name"]),
					Arguments: rawJSONString(output["arguments"]),
					Raw:       output,
				}
				if call.CallID == "" {
					call.CallID = call.ID
				}
				mergeToolCall(call, callsByKey, keys)
			}
		}
		if content, ok := sliceFromAny(value["content"]); ok {
			for _, contentAny := range content {
				block, ok := mapFromAny(contentAny)
				if !ok || stringFromAny(block["type"]) != "tool_use" {
					continue
				}
				mergeToolCall(SessionToolCall{
					ID:        stringFromAny(block["id"]),
					Type:      "tool_use",
					Name:      stringFromAny(block["name"]),
					Arguments: rawJSONString(block["input"]),
					Raw:       block,
				}, callsByKey, keys)
			}
		}
	}
}

func mergeToolCalls(value any, callsByKey map[string]*SessionToolCall, keys *[]string) {
	toolCalls, ok := sliceFromAny(value)
	if !ok {
		return
	}
	for _, toolAny := range toolCalls {
		tool, ok := mapFromAny(toolAny)
		if !ok {
			continue
		}
		fn, _ := mapFromAny(tool["function"])
		call := SessionToolCall{
			ID:        stringFromAny(tool["id"]),
			Type:      firstNonEmpty(stringFromAny(tool["type"]), "function"),
			Name:      stringFromAny(fn["name"]),
			Arguments: stringFromAny(fn["arguments"]),
			Index:     intFromAny(tool["index"]),
			Raw:       tool,
		}
		mergeToolCall(call, callsByKey, keys)
	}
}

func mergeToolCall(call SessionToolCall, callsByKey map[string]*SessionToolCall, keys *[]string) {
	key := firstNonEmpty(call.CallID, call.ID, call.Name)
	if key == "" && call.Index != 0 {
		key = fmt.Sprintf("index:%d", call.Index)
	}
	if key == "" {
		key = fmt.Sprintf("call:%d", len(*keys))
	}
	existing, ok := callsByKey[key]
	if !ok {
		callsByKey[key] = &call
		*keys = append(*keys, key)
		return
	}
	existing.ID = firstNonEmpty(existing.ID, call.ID)
	existing.CallID = firstNonEmpty(existing.CallID, call.CallID)
	existing.Type = firstNonEmpty(existing.Type, call.Type)
	existing.Name = firstNonEmpty(existing.Name, call.Name)
	existing.Arguments += call.Arguments
	if existing.Raw == nil {
		existing.Raw = call.Raw
	}
}

func usageFromPayload(payload any) *dto.Usage {
	payloadMap, ok := mapFromAny(payload)
	if !ok {
		return nil
	}
	if usage, ok := mapFromAny(payloadMap["usage"]); ok {
		return usageFromMap(usage)
	}
	if response, ok := mapFromAny(payloadMap["response"]); ok {
		if usage, ok := mapFromAny(response["usage"]); ok {
			return usageFromMap(usage)
		}
	}
	return nil
}

func usageFromMap(usage map[string]any) *dto.Usage {
	if len(usage) == 0 {
		return nil
	}
	result := &dto.Usage{
		PromptTokens:     intFromAny(usage["prompt_tokens"]),
		CompletionTokens: intFromAny(usage["completion_tokens"]),
		TotalTokens:      intFromAny(usage["total_tokens"]),
		InputTokens:      intFromAny(usage["input_tokens"]),
		OutputTokens:     intFromAny(usage["output_tokens"]),
	}
	if result.PromptTokens == 0 {
		result.PromptTokens = result.InputTokens
	}
	if result.CompletionTokens == 0 {
		result.CompletionTokens = result.OutputTokens
	}
	if result.TotalTokens == 0 {
		result.TotalTokens = result.PromptTokens + result.CompletionTokens
	}
	if result.PromptTokens == 0 && result.CompletionTokens == 0 && result.TotalTokens == 0 {
		return nil
	}
	return result
}

func streamStatusSnapshot(status *relaycommon.StreamStatus) *SessionArchiveStreamStatus {
	if status == nil {
		return nil
	}
	snapshot := &SessionArchiveStreamStatus{
		EndReason:  string(status.EndReason),
		ErrorCount: status.TotalErrorCount(),
		NormalEnd:  status.IsNormalEnd(),
	}
	if status.EndError != nil {
		snapshot.EndError = status.EndError.Error()
	}
	return snapshot
}

func mapFromAny(value any) (map[string]any, bool) {
	if value == nil {
		return nil, false
	}
	if m, ok := value.(map[string]any); ok {
		return m, true
	}
	m, err := common.Any2Type[map[string]any](value)
	if err != nil {
		return nil, false
	}
	return m, true
}

func sliceFromAny(value any) ([]any, bool) {
	if value == nil {
		return nil, false
	}
	if s, ok := value.([]any); ok {
		return s, true
	}
	s, err := common.Any2Type[[]any](value)
	if err != nil {
		return nil, false
	}
	return s, true
}

func stringFromAny(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return strings.TrimSpace(common.Interface2String(v))
	}
}

func intFromAny(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case int32:
		return int(v)
	case float64:
		return int(v)
	case float32:
		return int(v)
	case string:
		return common.String2Int(strings.TrimSpace(v))
	default:
		return 0
	}
}

func rawJSONString(value any) string {
	if value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	bytes, err := common.Marshal(value)
	if err != nil {
		return ""
	}
	return string(bytes)
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func sessionArchiveRawFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func boolFromAny(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true", "1", "yes", "y":
			return true, true
		case "false", "0", "no", "n":
			return false, true
		default:
			return false, false
		}
	case int:
		return typed != 0, true
	case int64:
		return typed != 0, true
	case int32:
		return typed != 0, true
	case float64:
		return typed != 0, true
	case float32:
		return typed != 0, true
	default:
		return false, false
	}
}

func cloneJSONValue(value any) any {
	if value == nil {
		return nil
	}
	data, err := common.Marshal(value)
	if err != nil {
		return value
	}
	var cloned any
	if err := common.Unmarshal(data, &cloned); err != nil {
		return value
	}
	return cloned
}

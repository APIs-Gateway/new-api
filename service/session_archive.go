package service

import (
	"bytes"
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
	sessionArchiveSchemaVersion = 1
	sessionArchiveRecordType    = "session_turn"
	sessionArchiveSubDir        = "session-jsonl"
)

var sessionArchiveWriteMu sync.Mutex

type SessionArchiveCapture struct {
	mu sync.Mutex

	startedAt        time.Time
	requestModelName string
	requestBody      string
	requestObject    any
	requestType      string

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
	SchemaVersion int    `json:"schema_version"`
	RecordType    string `json:"record_type"`

	SessionID       string `json:"session_id"`
	SessionIDSource string `json:"session_id_source,omitempty"`
	RequestID       string `json:"request_id,omitempty"`
	TurnComplete    bool   `json:"turn_complete"`
	StartedAt       int64  `json:"started_at,omitempty"`
	FinishedAt      int64  `json:"finished_at,omitempty"`

	RelayFormat             types.RelayFormat   `json:"relay_format,omitempty"`
	RelayMode               int                 `json:"relay_mode,omitempty"`
	RequestType             string              `json:"request_type,omitempty"`
	RequestPath             string              `json:"request_path,omitempty"`
	RequestMethod           string              `json:"request_method,omitempty"`
	RequestConversionChain  []types.RelayFormat `json:"request_conversion_chain,omitempty"`
	FinalRequestRelayFormat types.RelayFormat   `json:"final_request_relay_format,omitempty"`
	IsStream                bool                `json:"is_stream"`
	IsPlayground            bool                `json:"is_playground"`
	IsChannelTest           bool                `json:"is_channel_test"`

	UserID     int    `json:"user_id,omitempty"`
	Username   string `json:"username,omitempty"`
	Group      string `json:"group,omitempty"`
	UsingGroup string `json:"using_group,omitempty"`
	TokenID    int    `json:"token_id,omitempty"`
	TokenGroup string `json:"token_group,omitempty"`

	ChannelID       int    `json:"channel_id,omitempty"`
	ChannelType     int    `json:"channel_type,omitempty"`
	ChannelName     string `json:"channel_name,omitempty"`
	OriginModelName string `json:"origin_model_name,omitempty"`
	UpstreamModel   string `json:"upstream_model,omitempty"`

	RequestHeaders    map[string]string       `json:"request_headers,omitempty"`
	RequestBody       string                  `json:"request_body,omitempty"`
	RequestObject     any                     `json:"request_object,omitempty"`
	PromptCacheKey    string                  `json:"prompt_cache_key,omitempty"`
	PreviousResponseID string                 `json:"previous_response_id,omitempty"`
	ToolDefinitions  []SessionToolDefinition `json:"tool_definitions,omitempty"`
	ResponseHTTPStatus int                    `json:"response_http_status,omitempty"`
	ResponseID         string                 `json:"response_id,omitempty"`
	ResponseBody       string                 `json:"response_body,omitempty"`
	ResponseText       string                 `json:"response_text,omitempty"`
	ResponseUsage      *dto.Usage             `json:"response_usage,omitempty"`
	ResponseToolCalls  []SessionToolCall      `json:"response_tool_calls,omitempty"`
	ResponseWSMessages []SessionArchiveWSMessage `json:"response_ws_messages,omitempty"`
	ResponseError      string                    `json:"response_error,omitempty"`
	ResponseErrorType  string                    `json:"response_error_type,omitempty"`
	ResponseErrorCode  string                    `json:"response_error_code,omitempty"`
	ResponseStatusCode int                       `json:"response_status_code,omitempty"`
	StreamStatus      *SessionArchiveStreamStatus `json:"stream_status,omitempty"`
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
	if w != nil && w.capture != nil && n > 0 {
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
		if w != nil && w.capture != nil && n > 0 {
			w.capture.appendHTTPResponse([]byte(s[:n]))
		}
		return n, err
	}
	n, err := w.ResponseWriter.Write([]byte(s))
	if w != nil && w.capture != nil && n > 0 {
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
	startedAt := info.StartTime
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	capture := &SessionArchiveCapture{
		startedAt:       startedAt,
		requestModelName: requestModelName,
		requestBody:      rawRequestBody,
		requestObject:    cloneJSONValue(request),
		requestType:      fmt.Sprintf("%T", request),
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

	responseBody, wsMessages := capture.snapshotResponse()
	sessionID, sessionIDSource := resolveSessionID(c, info, request, capture.requestBody)
	promptCacheKey, previousResponseID := sessionLinkFields(request, capture.requestBody)
	record := &SessionArchiveRecord{
		SchemaVersion:          sessionArchiveSchemaVersion,
		RecordType:             sessionArchiveRecordType,
		SessionID:              sessionID,
		SessionIDSource:        sessionIDSource,
		RequestID:              info.RequestId,
		TurnComplete:           newAPIError == nil,
		StartedAt:              capture.startedAt.Unix(),
		FinishedAt:             time.Now().Unix(),
		RelayFormat:            info.RelayFormat,
		RelayMode:              info.RelayMode,
		RequestType:            capture.requestType,
		RequestPath:            info.RequestURLPath,
		RequestMethod:          c.Request.Method,
		RequestConversionChain: cloneRelayFormats(info.RequestConversionChain),
		FinalRequestRelayFormat: info.GetFinalRequestRelayFormat(),
		IsStream:               info.IsStream,
		IsPlayground:           info.IsPlayground,
		IsChannelTest:          info.IsChannelTest,
		UserID:                 info.UserId,
		Username:               c.GetString("username"),
		Group:                  info.UserGroup,
		UsingGroup:             info.UsingGroup,
		TokenID:                info.TokenId,
		TokenGroup:             info.TokenGroup,
		ChannelID:              common.GetContextKeyInt(c, constant.ContextKeyChannelId),
		ChannelType:            common.GetContextKeyInt(c, constant.ContextKeyChannelType),
		ChannelName:            common.GetContextKeyString(c, constant.ContextKeyChannelName),
		OriginModelName:        capture.requestModelName,
		UpstreamModel:          info.UpstreamModelName,
		RequestHeaders:         sanitizeSessionHeaders(info.RequestHeaders),
		RequestBody:            capture.requestBody,
		RequestObject:          capture.requestObject,
		PromptCacheKey:         promptCacheKey,
		PreviousResponseID:     previousResponseID,
		ToolDefinitions:        extractToolDefinitions(request),
		ResponseHTTPStatus:     c.Writer.Status(),
		ResponseID:             extractResponseID(responseBody),
		ResponseBody:           responseBody,
		ResponseText:           extractResponseText(responseBody),
		ResponseUsage:          extractResponseUsage(responseBody),
		ResponseToolCalls:      extractResponseToolCalls(responseBody),
		ResponseWSMessages:     wsMessages,
		StreamStatus:           streamStatusSnapshot(info.StreamStatus),
	}
	if newAPIError != nil {
		record.ResponseError = newAPIError.ErrorWithStatusCode()
		record.ResponseErrorType = string(newAPIError.GetErrorType())
		record.ResponseErrorCode = string(newAPIError.GetErrorCode())
		record.ResponseStatusCode = newAPIError.StatusCode
		if record.ResponseHTTPStatus == 0 {
			record.ResponseHTTPStatus = newAPIError.StatusCode
		}
	}

	if err := appendSessionArchiveRecord(record); err != nil {
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

func (c *SessionArchiveCapture) snapshotResponse() (string, []SessionArchiveWSMessage) {
	if c == nil {
		return "", nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	messages := make([]SessionArchiveWSMessage, len(c.wsMessages))
	copy(messages, c.wsMessages)
	return c.httpResponse.String(), messages
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

func appendSessionArchiveRecord(record *SessionArchiveRecord) error {
	if record == nil {
		return nil
	}
	line, err := common.Marshal(record)
	if err != nil {
		return err
	}
	path := sessionArchiveFilePath(record.OriginModelName, time.Unix(record.StartedAt, 0))
	sessionArchiveWriteMu.Lock()
	defer sessionArchiveWriteMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(append(line, '\n')); err != nil {
		return err
	}
	return nil
}

func sessionArchiveFilePath(modelName string, t time.Time) string {
	dir := strings.TrimSpace(common.SessionArchiveDir)
	if dir == "" {
		logDir := "./logs"
		if common.LogDir != nil && strings.TrimSpace(*common.LogDir) != "" {
			logDir = *common.LogDir
		}
		dir = filepath.Join(logDir, sessionArchiveSubDir)
	}
	if t.IsZero() {
		t = time.Now()
	}
	return filepath.Join(dir, sessionArchiveModelDir(modelName), fmt.Sprintf("session-%s.jsonl", t.Format("20060102")))
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
	if info != nil && strings.TrimSpace(info.RequestId) != "" {
		return info.RequestId, "request_id"
	}
	return common.GetTimeString(), "generated"
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
					builder.WriteString(stringFromAny(msg["reasoning_content"]))
					builder.WriteString(stringFromAny(msg["reasoning"]))
				}
				if delta, ok := mapFromAny(choice["delta"]); ok {
					builder.WriteString(contentText(delta["content"]))
					builder.WriteString(stringFromAny(delta["reasoning_content"]))
					builder.WriteString(stringFromAny(delta["reasoning"]))
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
		builder.WriteString(contentText(value["content"]))
		if delta, ok := mapFromAny(value["delta"]); ok {
			builder.WriteString(contentText(delta["text"]))
			builder.WriteString(contentText(delta["thinking"]))
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
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

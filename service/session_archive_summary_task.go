package service

import (
	"bufio"
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"

	"github.com/bytedance/gopkg/util/gopool"
)

const (
	sessionArchiveSummaryTaskInterval = 10 * time.Minute
	sessionArchiveSummaryDirName      = "summary"
	sessionArchiveSummaryFileName     = "summary.json"
	sessionArchiveScannerMaxLineBytes = 16 * 1024 * 1024
)

var (
	sessionArchiveSummaryOnce    sync.Once
	sessionArchiveSummaryRunning atomic.Bool
)

type SessionArchiveDailySummary struct {
	Date        string                               `json:"date"`
	GeneratedAt string                               `json:"generated_at,omitempty"`
	Models      map[string]*SessionArchiveModelStats `json:"models,omitempty"`
}

type SessionArchiveModelStats struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	SessionCount int64 `json:"session_count"`
}

func StartSessionArchiveDailySummaryTask() {
	sessionArchiveSummaryOnce.Do(func() {
		if !common.IsMasterNode || !common.SessionArchiveEnabled {
			return
		}
		gopool.Go(func() {
			logger.LogInfo(context.Background(), fmt.Sprintf("session archive summary task started: tick=%s", sessionArchiveSummaryTaskInterval))
			ticker := time.NewTicker(sessionArchiveSummaryTaskInterval)
			defer ticker.Stop()

			runSessionArchiveDailySummaryOnce(time.Now())
			for now := range ticker.C {
				runSessionArchiveDailySummaryOnce(now)
			}
		})
	})
}

func runSessionArchiveDailySummaryOnce(now time.Time) {
	if !sessionArchiveSummaryRunning.CompareAndSwap(false, true) {
		return
	}
	defer sessionArchiveSummaryRunning.Store(false)

	targetDay := now.AddDate(0, 0, -1)
	if err := writeSessionArchiveDailySummary(targetDay, now); err != nil {
		logger.LogWarn(context.Background(), fmt.Sprintf("session archive summary task failed: %v", err))
	}
}

func writeSessionArchiveDailySummary(targetDay time.Time, generatedAt time.Time) error {
	summary, err := buildSessionArchiveDailySummary(targetDay, generatedAt)
	if err != nil {
		return err
	}
	content, err := common.Marshal(summary)
	if err != nil {
		return err
	}
	path := sessionArchiveSummaryFilePath(targetDay)
	sessionArchiveWriteMu.Lock()
	defer sessionArchiveWriteMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, content, 0644)
}

func buildSessionArchiveDailySummary(targetDay time.Time, generatedAt time.Time) (*SessionArchiveDailySummary, error) {
	rootDir := sessionArchiveRootDir()
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		if os.IsNotExist(err) {
			return &SessionArchiveDailySummary{
				Date:        targetDay.Format("2006-01-02"),
				GeneratedAt: generatedAt.Format(time.RFC3339),
				Models:      map[string]*SessionArchiveModelStats{},
			}, nil
		}
		return nil, err
	}

	summary := &SessionArchiveDailySummary{
		Date:        targetDay.Format("2006-01-02"),
		GeneratedAt: generatedAt.Format(time.RFC3339),
		Models:      make(map[string]*SessionArchiveModelStats),
	}
	fileName := fmt.Sprintf("session-%s.jsonl", targetDay.Format("20060102"))
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == sessionArchiveSummaryDirName {
			continue
		}
		modelName, err := url.PathUnescape(entry.Name())
		if err != nil || strings.TrimSpace(modelName) == "" {
			modelName = entry.Name()
		}
		if err := accumulateSessionArchiveSummaryFromFile(filepath.Join(rootDir, entry.Name(), fileName), modelName, summary); err != nil {
			return nil, err
		}
	}
	return summary, nil
}

func accumulateSessionArchiveSummaryFromFile(path string, fallbackModelName string, summary *SessionArchiveDailySummary) error {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), sessionArchiveScannerMaxLineBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var record SessionArchiveRecord
		if err := common.Unmarshal([]byte(line), &record); err != nil {
			common.SysError(fmt.Sprintf("failed to parse session archive record for summary: %s, path=%s", err.Error(), path))
			continue
		}
		modelName := firstNonEmpty(record.OriginModelName, fallbackModelName)
		if modelName == "" {
			modelName = "unknown-model"
		}
		stats := summary.Models[modelName]
		if stats == nil {
			stats = &SessionArchiveModelStats{}
			summary.Models[modelName] = stats
		}
		inputTokens, outputTokens, totalTokens := sessionArchiveSummaryTokens(&record)
		stats.InputTokens += int64(inputTokens)
		stats.OutputTokens += int64(outputTokens)
		stats.TotalTokens += int64(totalTokens)
		stats.SessionCount++
	}
	return scanner.Err()
}

func sessionArchiveSummaryTokens(record *SessionArchiveRecord) (int, int, int) {
	if record == nil {
		return 0, 0, 0
	}
	inputTokens := record.PromptTokens
	outputTokens := record.CompletionTokens
	totalTokens := record.TotalTokens
	if record.ResponseUsage != nil {
		if inputTokens == 0 {
			inputTokens = record.ResponseUsage.PromptTokens
		}
		if outputTokens == 0 {
			outputTokens = record.ResponseUsage.CompletionTokens
		}
		if totalTokens == 0 {
			totalTokens = record.ResponseUsage.TotalTokens
		}
	}
	if totalTokens == 0 {
		totalTokens = inputTokens + outputTokens
	}
	return inputTokens, outputTokens, totalTokens
}

func sessionArchiveSummaryFilePath(targetDay time.Time) string {
	return filepath.Join(sessionArchiveRootDir(), sessionArchiveSummaryDirName, targetDay.Format("20060102"), sessionArchiveSummaryFileName)
}

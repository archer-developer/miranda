package agentloop

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/archer-developer/miranda-llm/llmtrace/analyze"
	"github.com/archer-developer/miranda-llm/llmtrace/anomaly"
)

// AnomalyConfig configures per-turn anomaly detection (see runAgentLoop's use
// of an anomaly.Recorder and Handle's reportAnomalies call once a turn
// ends). The zero value disables the feature entirely.
type AnomalyConfig struct {
	// LLMLogPath is llm.log's own path, re-read at anomaly time via
	// analyze.ParseAll to recover the whole conversation's blocks for the
	// anomaly file, not just this one turn's — diagnosing a bad turn often
	// needs the history that led up to it. Best-effort: if the read fails,
	// the anomaly file falls back to just this turn's own blocks.
	LLMLogPath string
	// Dir is where an anomalous turn's blocks get written, one file per
	// anomalous turn (see llmtrace/anomaly.FileName) — created lazily on
	// first use.
	Dir string
	// MaxAge is how long to keep a file under Dir before CleanOldAnomalies
	// deletes it. Unlike miranda.log/llm.log, anomaly files aren't
	// size-rotated by lumberjack — one is written per anomalous turn and
	// otherwise kept forever, so a long-lived process with frequent
	// anomalies grows Dir unbounded without this. Zero means no cleanup.
	MaxAge time.Duration
}

func (c AnomalyConfig) enabled() bool { return c.Dir != "" }

// SetAnomalyConfig enables (or, with the zero value, disables) per-turn
// anomaly detection — mirrors SetLogger/SetSchedule's post-construction
// wiring style for an optional dependency. Leaving it uncalled (the
// default) means Handle never attaches a Recorder to a turn's ctx at all.
func (o *Orchestrator) SetAnomalyConfig(cfg AnomalyConfig) {
	o.anomaly = cfg
}

// reportAnomalies runs anomaly.Detect over one turn's recorded blocks and,
// if it finds anything, writes the fuller conversation context (see
// AnomalyConfig.LLMLogPath) to a new file under AnomalyConfig.Dir and logs
// exactly one WARNING to the app logger — never the full trace itself,
// that's what the file is for. A no-op when recorder is nil (anomaly
// detection disabled — see Handle) or o.logger is nil (matches every other
// optional-logger call site in this package, e.g. mcp_dispatch.go).
func (o *Orchestrator) reportAnomalies(conversationID string, recorder *anomaly.Recorder, outcome anomaly.Outcome) {
	if recorder == nil {
		return
	}

	found := anomaly.Detect(recorder.Blocks(), recorder.Durations(), outcome, anomaly.Options{})
	if len(found) == 0 {
		return
	}

	blocks := recorder.Blocks()
	if conversationID != "" {
		all, err := readLLMLog(o.anomaly.LLMLogPath)
		if err != nil {
			if o.logger != nil {
				o.logger.Warn("orchestrator: re-reading llm.log for anomaly context failed, writing turn-only blocks", "error", err)
			}
		} else if conv := analyze.ConversationBlocks(all, conversationID); len(conv) > 0 {
			blocks = conv
		}
	}

	path, err := writeAnomalyFile(o.anomaly.Dir, found, blocks)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("orchestrator: failed to write anomaly file", "error", err)
		}
		return
	}
	if o.logger != nil {
		o.logger.Warn("orchestrator: turn had anomalies", "conversationId", conversationID, "kinds", anomalyKinds(found), "file", path)
	}
}

func readLLMLog(path string) ([]analyze.Block, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: open llm log: %w", err)
	}
	defer func() { _ = f.Close() }()
	return analyze.ParseAll(f)
}

func writeAnomalyFile(dir string, found []anomaly.Anomaly, blocks []analyze.Block) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("orchestrator: create anomalies dir: %w", err)
	}
	path := filepath.Join(dir, anomaly.FileName(time.Now(), found))
	f, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("orchestrator: create anomaly file: %w", err)
	}
	if err := anomaly.WriteFile(f, found, blocks); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("orchestrator: close anomaly file: %w", err)
	}
	return path, nil
}

// CleanOldAnomalies deletes files under o.anomaly.Dir whose modification
// time is older than o.anomaly.MaxAge — see AnomalyConfig.MaxAge for why
// this exists (anomaly files aren't size-rotated the way miranda.log/llm.log
// are). A no-op if anomaly detection is disabled (Dir == "") or MaxAge is
// zero (no age-based cleanup configured). Meant to be called periodically by
// a background sweep (see cmd/miranda's sweepAnomalies), not per-turn.
func (o *Orchestrator) CleanOldAnomalies() error {
	if !o.anomaly.enabled() || o.anomaly.MaxAge <= 0 {
		return nil
	}

	entries, err := os.ReadDir(o.anomaly.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("orchestrator: read anomalies dir %s: %w", o.anomaly.Dir, err)
	}

	cutoff := time.Now().Add(-o.anomaly.MaxAge)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // file may have been removed concurrently; nothing to clean up
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filepath.Join(o.anomaly.Dir, e.Name())); err != nil {
				return fmt.Errorf("orchestrator: remove old anomaly file %s: %w", e.Name(), err)
			}
		}
	}
	return nil
}

func anomalyKinds(found []anomaly.Anomaly) []string {
	seen := map[string]bool{}
	var kinds []string
	for _, a := range found {
		if !seen[a.Kind] {
			seen[a.Kind] = true
			kinds = append(kinds, a.Kind)
		}
	}
	return kinds
}

package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/archer-developer/miranda/internal/hub"
)

// Voicing for replies that did NOT come through Home Assistant's Assist
// pipeline (Telegram, web UI, CLI, scheduled reminders) is delegated to the
// miranda-tts service over MCP: it synthesizes with Gemini and plays on a
// Home Assistant satellite or media player. Miranda's part is only policy
// and addressing — whether to speak, and which concrete entity_id to target
// (it resolves friendly names; miranda-tts only ever sees entity_ids).
//
// ha_assist turns never come through here: the pipeline's own TTS engine
// (miranda-tts again, over Wyoming) speaks that reply exactly once.
const (
	// ttsServerName is the mcp.servers entry for miranda-tts. Its own
	// speak/stop tools are hidden from the model (see availableTools);
	// speak_reply/stop_speech are the model-facing wrappers.
	ttsServerName = "miranda-tts"
	// Prefixed MCP tool names, "<server>_<tool>" (see mcp.prefixedToolName).
	ttsSpeakTool = ttsServerName + "_speak"
	ttsStopTool  = ttsServerName + "_stop"

	// speakCallTimeout bounds one miranda-tts speak call. The tool blocks
	// until playback finishes, so it must outlast a long reply.
	speakCallTimeout = 10 * time.Minute
	// stopCallTimeout bounds one miranda-tts stop call.
	stopCallTimeout = 15 * time.Second
	// recentSpeechWindow is how long after a speak call returns that
	// stop_speech still targets the entity, because playback can outlast the
	// call (a timeout, or an MCP transport that gave up before playback ended).
	recentSpeechWindow = 30 * time.Minute
)

// ttsAvailable reports whether miranda-tts is configured and connected — the
// precondition for offering speak_reply/stop_speech to the model.
func (o *Orchestrator) ttsAvailable() bool {
	return o.tools != nil && o.tools.HasClient(ttsServerName)
}

// resolveSpeakerEntity maps device (a friendly name; empty means the
// configured default device) to the entity_id miranda-tts should target.
func (o *Orchestrator) resolveSpeakerEntity(ctx context.Context, device string) (string, error) {
	target := device
	if target == "" {
		target = o.ttsCfg.DefaultDevice
	}
	if target == "" {
		return "", errors.New("no speaker device specified and tts.default_device is not configured")
	}
	if o.speakerHA == nil {
		return "", errors.New("cannot resolve speaker names: Home Assistant (HA_BASE_URL) is not configured")
	}
	entityID, err := o.speakerHA.ResolveSpeaker(ctx, target)
	if err != nil {
		return "", fmt.Errorf("resolve speaker %q: %w", target, err)
	}
	return entityID, nil
}

// speakJob is one queued miranda-tts speak call.
type speakJob struct {
	entityID string
	args     string
}

// speakOnEntity queues text to be spoken on entityID by miranda-tts without
// waiting for playback: the call runs in a background goroutine under a
// context detached from the turn (the turn ends long before a long utterance
// does), and a failure is published to the hub (Source: "tts") rather than
// returned — the same fire-and-forget contract the in-process TTS queue had,
// so a broken speaker never stalls or fails an answer.
//
// Utterances for the same entity play strictly in order: miranda-tts treats a
// new speak on an entity as an interruption of the running one, so two
// back-to-back speak_reply calls (or a reminder firing mid-reply) must not be
// sent concurrently.
func (o *Orchestrator) speakOnEntity(ctx context.Context, entityID, text string) {
	args, err := json.Marshal(map[string]string{"text": text, "entity_id": entityID})
	if err != nil {
		o.hub.Publish(hub.Event{Source: "tts", Message: "speak: marshal arguments: " + err.Error()})
		return
	}

	o.speakingMu.Lock()
	if o.speakQueue == nil {
		o.speakQueue = make(map[string][]speakJob)
		o.speakRunning = make(map[string]bool)
		o.lastSpoke = make(map[string]time.Time)
	}
	o.speakQueue[entityID] = append(o.speakQueue[entityID], speakJob{entityID: entityID, args: string(args)})
	o.addSpeakingLocked(entityID, 1)
	start := !o.speakRunning[entityID]
	if start {
		o.speakRunning[entityID] = true
	}
	o.speakingMu.Unlock()

	if start {
		go o.drainSpeakQueue(context.WithoutCancel(ctx), entityID)
	}
}

// drainSpeakQueue runs entityID's queued speak jobs one at a time until the
// queue is empty, then exits (the next speakOnEntity starts a new drainer).
func (o *Orchestrator) drainSpeakQueue(ctx context.Context, entityID string) {
	for {
		o.speakingMu.Lock()
		queue := o.speakQueue[entityID]
		if len(queue) == 0 {
			delete(o.speakQueue, entityID)
			delete(o.speakRunning, entityID)
			o.speakingMu.Unlock()
			return
		}
		job := queue[0]
		o.speakQueue[entityID] = queue[1:]
		o.speakingMu.Unlock()

		callCtx, cancel := context.WithTimeout(ctx, speakCallTimeout)
		if _, err := o.tools.Call(callCtx, ttsSpeakTool, job.args); err != nil {
			o.hub.Publish(hub.Event{Source: "tts", Message: fmt.Sprintf("speak on %s failed: %v", entityID, err)})
		}
		cancel()

		o.speakingMu.Lock()
		o.lastSpoke[entityID] = time.Now()
		o.addSpeakingLocked(entityID, -1)
		o.speakingMu.Unlock()
	}
}

// speakChecked speaks text on the default device. Unlike speak_reply it
// reports the one failure detectable before playback — no speaker could be
// resolved or miranda-tts isn't connected — so a reminder firing can record
// an honest outcome. Playback itself stays asynchronous and its failures
// remain undetectable here, as with speak_reply.
func (o *Orchestrator) speakChecked(ctx context.Context, text string) error {
	if text == "" {
		return nil
	}
	if !o.ttsAvailable() {
		return fmt.Errorf("tts: %s MCP server is not connected", ttsServerName)
	}
	entityID, err := o.resolveSpeakerEntity(ctx, "")
	if err != nil {
		o.hub.Publish(hub.Event{Source: "tts", Message: "no TTS device configured or could not be resolved — skipping speech: " + err.Error()})
		return fmt.Errorf("tts: %w", err)
	}
	o.speakOnEntity(ctx, entityID, text)
	return nil
}

// stopSpeech interrupts speech on every entity a speak_reply is still
// running or queued on, every entity spoken on within recentSpeechWindow
// (playback can outlive the speak call that started it, e.g. when that call
// timed out), plus the default device (so a stop also reaches speech that
// outlived a restart or came from a reminder). Queued utterances are
// discarded first so they don't start right after the stop. It returns the
// model-facing tool result.
func (o *Orchestrator) stopSpeech(ctx context.Context) string {
	targets := o.takeStopTargets()
	if entityID, err := o.resolveSpeakerEntity(ctx, ""); err == nil {
		targets[entityID] = true
	}
	if len(targets) == 0 {
		return "nothing to stop: no speaker is active and no default device is configured"
	}

	var failures []string
	for entityID := range targets {
		callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopCallTimeout)
		args, _ := json.Marshal(map[string]string{"entity_id": entityID})
		_, err := o.tools.Call(callCtx, ttsStopTool, string(args))
		cancel()
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", entityID, err))
		}
	}
	if len(failures) > 0 {
		return "error: could not stop speech: " + strings.Join(failures, "; ")
	}
	return "stopped"
}

// takeStopTargets drops every queued (not yet started) speak job and returns
// the set of entities stop_speech must interrupt: those with a speak queued
// or in flight, and those spoken on recently.
func (o *Orchestrator) takeStopTargets() map[string]bool {
	o.speakingMu.Lock()
	defer o.speakingMu.Unlock()

	targets := make(map[string]bool, len(o.speaking))
	for id := range o.speaking {
		targets[id] = true
	}
	for id, at := range o.lastSpoke {
		if time.Since(at) <= recentSpeechWindow {
			targets[id] = true
		}
	}
	for id, queue := range o.speakQueue {
		// The job currently running was already popped, so everything left
		// in the queue is waiting and can simply be dropped.
		o.addSpeakingLocked(id, -len(queue))
		delete(o.speakQueue, id)
	}
	return targets
}

// addSpeakingLocked adjusts the queued-or-in-flight speak count for
// entityID. Must be called with o.speakingMu held.
func (o *Orchestrator) addSpeakingLocked(entityID string, delta int) {
	if o.speaking == nil {
		o.speaking = make(map[string]int)
	}
	o.speaking[entityID] += delta
	if o.speaking[entityID] <= 0 {
		delete(o.speaking, entityID)
	}
}

// speakingEntities returns a copy of the set of entities with a speak call
// queued or in flight.
func (o *Orchestrator) speakingEntities() map[string]bool {
	o.speakingMu.Lock()
	defer o.speakingMu.Unlock()
	out := make(map[string]bool, len(o.speaking))
	for id := range o.speaking {
		out[id] = true
	}
	return out
}

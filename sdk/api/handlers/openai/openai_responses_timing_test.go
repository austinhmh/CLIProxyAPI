package openai

import (
	"bytes"
	"errors"
	"testing"
	"time"

	requestlogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

type failingResponsesTimingWriter struct{}

func (failingResponsesTimingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestResponsesSSEFramerRecordsFirstEventAndVisibleText(t *testing.T) {
	tracker := requestlogging.NewRequestTimingTracker("request-1", "POST /v1/responses", time.Now())
	turn := tracker.BeginTurn("http_stream", "gpt-5.6-luna")
	framer := &responsesSSEFramer{timingTurn: turn}

	var output bytes.Buffer
	framer.WriteChunk(&output, []byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n"))
	framer.WriteChunk(&output, []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n"))
	turn.Complete("completed")

	snapshot := tracker.Snapshot()
	if len(snapshot.Turns) != 1 {
		t.Fatalf("turn count = %d, want 1", len(snapshot.Turns))
	}
	firstEventCount := 0
	firstVisibleTextCount := 0
	for _, event := range snapshot.Turns[0].Events {
		switch event.Stage {
		case requestlogging.TimingStageFirstDownstreamEvent:
			firstEventCount++
			if event.Outcome != "response.created" {
				t.Fatalf("first downstream event outcome = %q, want response.created", event.Outcome)
			}
		case requestlogging.TimingStageFirstVisibleText:
			firstVisibleTextCount++
		}
	}
	if firstEventCount != 1 {
		t.Fatalf("first downstream event count = %d, want 1", firstEventCount)
	}
	if firstVisibleTextCount != 1 {
		t.Fatalf("first visible text count = %d, want 1", firstVisibleTextCount)
	}
}

func TestResponsesSSEFramerIgnoresEmptyTextDelta(t *testing.T) {
	tracker := requestlogging.NewRequestTimingTracker("request-2", "POST /v1/responses", time.Now())
	turn := tracker.BeginTurn("http_stream", "gpt-5.6-luna")
	framer := &responsesSSEFramer{timingTurn: turn}

	var output bytes.Buffer
	framer.WriteChunk(&output, []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"\"}\n\n"))
	turn.Complete("completed")

	for _, event := range tracker.Snapshot().Turns[0].Events {
		if event.Stage == requestlogging.TimingStageFirstVisibleText {
			t.Fatal("empty text delta recorded as visible text")
		}
	}
}

func TestResponsesSSEFramerDoesNotRecordDownstreamEventWhenWriteFails(t *testing.T) {
	tracker := requestlogging.NewRequestTimingTracker("request-3", "POST /v1/responses", time.Now())
	turn := tracker.BeginTurn("http_stream", "gpt-5.6-luna")
	framer := &responsesSSEFramer{timingTurn: turn}

	framer.WriteChunk(failingResponsesTimingWriter{}, []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n"))
	turn.Complete("failed")

	for _, event := range tracker.Snapshot().Turns[0].Events {
		switch event.Stage {
		case requestlogging.TimingStageFirstDownstreamEvent, requestlogging.TimingStageFirstVisibleText:
			t.Fatalf("failed write recorded downstream event: %#v", event)
		}
	}
}

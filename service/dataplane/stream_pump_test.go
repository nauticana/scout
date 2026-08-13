package dataplane

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
	"github.com/nauticana/scout/internal/stage"
)

type publishRecorder struct {
	frames []domain.TurnReply
	fail   error
}

func (recorder *publishRecorder) Publish(_ context.Context, reply domain.TurnReply) error {
	if recorder.fail != nil {
		return recorder.fail
	}
	recorder.frames = append(recorder.frames, reply)
	return nil
}

func chunkStream(chunks ...domain.ModelChunk) *fake.ModelStream {
	index := 0
	return &fake.ModelStream{
		ReceiveFunc: func(context.Context) (domain.ModelChunk, error) {
			if index >= len(chunks) {
				return domain.ModelChunk{}, io.EOF
			}
			chunk := chunks[index]
			index++
			return chunk, nil
		},
		CloseFunc: func() error { return nil },
	}
}

var pumpTurn = domain.TurnRequest{
	TenantContext:  domain.TenantContext{TenantID: 1},
	RequestID:      "r",
	ConversationID: "c",
}

func TestStreamPumpGuardsAndPublishesEveryChunk(t *testing.T) {
	recorder := &publishRecorder{}
	guarded := 0
	pump := &StreamPump{
		Guardrails: &fake.GuardrailEnforcer{AfterModelChunkFunc: func(_ context.Context, _ domain.GuardrailConfig, chunk domain.ModelChunk) (domain.ModelChunk, error) {
			guarded++
			chunk.Payload = append([]byte("ok:"), chunk.Payload...)
			return chunk, nil
		}},
		Publisher: recorder,
	}
	usage, err := pump.Run(context.Background(), pumpTurn, "route", "v1", domain.GuardrailConfig{}, chunkStream(
		domain.ModelChunk{Payload: []byte("a"), Usage: domain.Usage{OutputTokens: 1}},
		domain.ModelChunk{Payload: []byte("b"), FinishReason: "stop", Usage: domain.Usage{OutputTokens: 1}},
	))
	if err != nil {
		t.Fatal(err)
	}
	if usage.OutputTokens != 2 || guarded != 2 || len(recorder.frames) != 2 {
		t.Fatalf("usage=%+v guarded=%d frames=%d", usage, guarded, len(recorder.frames))
	}
	if string(recorder.frames[0].Payload) != "ok:a" || recorder.frames[0].Final {
		t.Fatalf("first frame = %+v", recorder.frames[0])
	}
	last := recorder.frames[1]
	if !last.Final || last.Sequence != 1 || last.AgentVersion != "v1" || last.ReplyRoute != "route" {
		t.Fatalf("final frame = %+v", last)
	}
}

func TestStreamPumpSynthesizesFinalOnBareEOF(t *testing.T) {
	recorder := &publishRecorder{}
	pump := &StreamPump{Guardrails: &fake.GuardrailEnforcer{}, Publisher: recorder}
	if _, err := pump.Run(context.Background(), pumpTurn, "route", "v1", domain.GuardrailConfig{}, chunkStream()); err != nil {
		t.Fatal(err)
	}
	if len(recorder.frames) != 1 || !recorder.frames[0].Final {
		t.Fatalf("frames = %+v", recorder.frames)
	}
}

func TestStreamPumpTruncatesAtTokenBudget(t *testing.T) {
	recorder := &publishRecorder{}
	closed := false
	stream := &fake.ModelStream{
		ReceiveFunc: func(context.Context) (domain.ModelChunk, error) {
			return domain.ModelChunk{Payload: []byte("x"), Usage: domain.Usage{OutputTokens: 5}}, nil
		},
		CloseFunc: func() error { closed = true; return nil },
	}
	pump := &StreamPump{Guardrails: &fake.GuardrailEnforcer{}, Publisher: recorder, MaxOutputTokens: 5}
	usage, err := pump.Run(context.Background(), pumpTurn, "route", "v1", domain.GuardrailConfig{}, stream)
	if err != nil {
		t.Fatal(err)
	}
	if usage.OutputTokens != 5 || !closed {
		t.Fatalf("usage=%+v closed=%v", usage, closed)
	}
	last := recorder.frames[len(recorder.frames)-1]
	if !last.Final || last.ErrorCode != ErrorCodeTruncated {
		t.Fatalf("final = %+v", last)
	}
}

func TestStreamPumpDropsChunkThatOvershootsTokenBudget(t *testing.T) {
	recorder := &publishRecorder{}
	pump := &StreamPump{Guardrails: &fake.GuardrailEnforcer{}, Publisher: recorder, MaxOutputTokens: 5}
	usage, err := pump.Run(context.Background(), pumpTurn, "route", "v1", domain.GuardrailConfig{}, chunkStream(
		domain.ModelChunk{Payload: []byte("too much"), Usage: domain.Usage{OutputTokens: 6}},
	))
	if err != nil || usage.OutputTokens != 6 {
		t.Fatalf("usage=%+v error=%v", usage, err)
	}
	if len(recorder.frames) != 1 || len(recorder.frames[0].Payload) != 0 || recorder.frames[0].ErrorCode != ErrorCodeTruncated {
		t.Fatalf("frames = %+v", recorder.frames)
	}
}

func TestStreamPumpStageAttribution(t *testing.T) {
	guardrailErr := errors.New("blocked")
	pump := &StreamPump{
		Guardrails: &fake.GuardrailEnforcer{AfterModelChunkFunc: func(_ context.Context, _ domain.GuardrailConfig, _ domain.ModelChunk) (domain.ModelChunk, error) {
			return domain.ModelChunk{}, guardrailErr
		}},
		Publisher: &publishRecorder{},
	}
	_, err := pump.Run(context.Background(), pumpTurn, "route", "v1", domain.GuardrailConfig{}, chunkStream(domain.ModelChunk{Payload: []byte("a")}))
	var stageErr *stage.Error
	if !errors.As(err, &stageErr) || stageErr.Stage != domain.StageGuardrail || !errors.Is(err, guardrailErr) {
		t.Fatalf("guardrail stage = %v", err)
	}

	modelErr := errors.New("provider down")
	pump = &StreamPump{Guardrails: &fake.GuardrailEnforcer{}, Publisher: &publishRecorder{}}
	stream := &fake.ModelStream{
		ReceiveFunc: func(context.Context) (domain.ModelChunk, error) { return domain.ModelChunk{}, modelErr },
		CloseFunc:   func() error { return nil },
	}
	_, err = pump.Run(context.Background(), pumpTurn, "route", "v1", domain.GuardrailConfig{}, stream)
	if !errors.As(err, &stageErr) || stageErr.Stage != domain.StageModel {
		t.Fatalf("model stage = %v", err)
	}

	publishErr := errors.New("broker down")
	pump = &StreamPump{Guardrails: &fake.GuardrailEnforcer{}, Publisher: &publishRecorder{fail: publishErr}}
	_, err = pump.Run(context.Background(), pumpTurn, "route", "v1", domain.GuardrailConfig{}, chunkStream(domain.ModelChunk{Payload: []byte("a")}))
	if !errors.As(err, &stageErr) || stageErr.Stage != domain.StagePublish {
		t.Fatalf("publish stage = %v", err)
	}
}

func TestStreamPumpGuardrailFailurePublishesNoPayload(t *testing.T) {
	recorder := &publishRecorder{}
	pump := &StreamPump{
		Guardrails: &fake.GuardrailEnforcer{AfterModelChunkFunc: func(_ context.Context, _ domain.GuardrailConfig, _ domain.ModelChunk) (domain.ModelChunk, error) {
			return domain.ModelChunk{}, errors.New("blocked")
		}},
		Publisher: recorder,
	}
	pump.Run(context.Background(), pumpTurn, "route", "v1", domain.GuardrailConfig{}, chunkStream(domain.ModelChunk{Payload: []byte("secret")}))
	for _, published := range recorder.frames {
		if len(published.Payload) != 0 {
			t.Fatalf("raw payload escaped: %+v", published)
		}
	}
}
